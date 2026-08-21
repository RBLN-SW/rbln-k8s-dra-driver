/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/urfave/cli/v2"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/consts"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
)

type Flags struct {
	certFile   string
	keyFile    string
	port       int
	driverName string
}

func main() {
	// The contract logger comes first so everything below emits through it.
	level, format := logging.SetupFromEnv()
	// Route klog (apimachinery, client-go) through slog, V(n) gate included.
	logging.BridgeKlog(level)

	if err := newApp(level, format).Run(os.Args); err != nil {
		slog.Error("Command failed", "err", err)
		os.Exit(1)
	}
}

func newApp(logLevel, logFormat string) *cli.App {
	flags := &Flags{}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "tls-cert-file",
			Usage:       "File containing the default x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert).",
			Destination: &flags.certFile,
			Required:    true,
		},
		&cli.StringFlag{
			Name:        "tls-private-key-file",
			Usage:       "File containing the default x509 private key matching --tls-cert-file.",
			Destination: &flags.keyFile,
			Required:    true,
		},
		&cli.IntFlag{
			Name:        "port",
			Usage:       "Secure port that the webhook listens on",
			Value:       443,
			Destination: &flags.port,
		},
		&cli.StringFlag{
			Name:        "driver-name",
			Usage:       "Name of the DRA driver.",
			Destination: &flags.driverName,
			EnvVars:     []string{"DRIVER_NAME"},
		},
	}
	app := &cli.App{
		Name:            "webhook",
		Usage:           "webhook implements a validating admission webhook complementing a DRA driver plugin.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return nil
		},
		Action: func(c *cli.Context) error {
			if flags.driverName == "" {
				flags.driverName = consts.DriverName
			}

			mux, err := newMux(flags.driverName)
			if err != nil {
				return fmt.Errorf("create HTTP mux: %w", err)
			}

			server := &http.Server{
				Handler: mux,
				Addr:    fmt.Sprintf(":%d", flags.port),
				// net/http logs TLS handshake failures and handler panics
				// through this logger. Left unset it is the stdlib default,
				// which writes unstructured text to stderr — so the records
				// that matter most on a webhook (a cert the apiserver will not
				// accept) would be the only ones outside the contract.
				ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
			}
			slog.Info("Starting webhook server",
				"addr", server.Addr, "driverName", flags.driverName,
				"logLevel", logLevel, "logFormat", logFormat)
			return server.ListenAndServeTLS(flags.certFile, flags.keyFile)
		},
	}

	return app
}

func newMux(driverName string) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/validate-resource-claim-parameters", serveResourceClaim(driverName))
	mux.HandleFunc("/readyz", readyHandler)
	return mux, nil
}

func readyHandler(w http.ResponseWriter, req *http.Request) {
	_, err := w.Write([]byte("ok"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func serveResourceClaim(driverName string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, admitResourceClaimParameters(driverName))
	}
}

func serve(w http.ResponseWriter, r *http.Request, admit func(context.Context, admissionv1.AdmissionReview) *admissionv1.AdmissionResponse) {
	ctx := r.Context()
	logger := logging.FromContext(ctx)

	var body []byte
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Error("Failed to read request body", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body = data
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		msg := fmt.Sprintf("contentType=%s, expected application/json", contentType)
		logger.Warn("Rejected request with unexpected content type", "contentType", contentType)
		http.Error(w, msg, http.StatusUnsupportedMediaType)
		return
	}

	// The raw body carries arbitrary user object contents, so it stays behind
	// the trace gate; debug is documented as production-usable.
	logger.Log(ctx, logging.LevelTrace, "Handling admission request", "body", string(body))

	requestedAdmissionReview, err := readAdmissionReview(body)
	if err != nil {
		msg := fmt.Sprintf("failed to read AdmissionReview from request body: %v", err)
		logger.Warn("Failed to read AdmissionReview from request body", "err", err)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Every record below inherits these, so the rejection warns do not each
	// have to restate them and nothing on this path is left unattributable.
	req := requestedAdmissionReview.Request
	ctx = logging.WithValues(ctx, "requestUID", string(req.UID),
		"resource", req.Resource.String(), "namespace", req.Namespace, "name", req.Name)
	logger = logging.FromContext(ctx)

	responseAdmissionReview := &admissionv1.AdmissionReview{}
	responseAdmissionReview.SetGroupVersionKind(requestedAdmissionReview.GroupVersionKind())
	responseAdmissionReview.Response = admit(ctx, *requestedAdmissionReview)
	responseAdmissionReview.Response.UID = req.UID

	logger.Debug("Sending admission response", "allowed", responseAdmissionReview.Response.Allowed)
	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		logger.Error("Failed to marshal admission response", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		logger.Error("Failed to write admission response", "err", err)
	}
}

func readAdmissionReview(data []byte) (*admissionv1.AdmissionReview, error) {
	deserializer := codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be decoded: %w", err)
	}

	if *gvk != admissionv1.SchemeGroupVersion.WithKind("AdmissionReview") {
		return nil, fmt.Errorf("unsupported group version kind: %v", gvk)
	}

	requestedAdmissionReview, ok := obj.(*admissionv1.AdmissionReview)
	if !ok {
		return nil, fmt.Errorf("expected v1.AdmissionReview but got: %T", obj)
	}
	// A review without a request decodes cleanly, and every caller dereferences
	// Request. Rejecting it here turns a handler panic — which net/http reports
	// as an unstructured stack trace and a bare 500 — into a warn and a 400.
	if requestedAdmissionReview.Request == nil {
		return nil, fmt.Errorf("AdmissionReview carries no request")
	}

	return requestedAdmissionReview, nil
}

func admitResourceClaimParameters(_ string) func(ctx context.Context, ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	return func(ctx context.Context, ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
		logger := logging.FromContext(ctx)

		switch ar.Request.Resource {
		case resourceClaimResourceV1, resourceClaimResourceV1Beta1, resourceClaimResourceV1Beta2:
			_, extractErr := extractResourceClaim(ar)
			if extractErr != nil {
				logger.Warn("Rejected resource claim parameters", "err", extractErr)
				return &admissionv1.AdmissionResponse{
					Result: &metav1.Status{
						Message: extractErr.Error(),
						Reason:  metav1.StatusReasonBadRequest,
					},
				}
			}
		case resourceClaimTemplateResourceV1, resourceClaimTemplateResourceV1Beta1, resourceClaimTemplateResourceV1Beta2:
			_, extractErr := extractResourceClaimTemplate(ar)
			if extractErr != nil {
				logger.Warn("Rejected resource claim template parameters", "err", extractErr)
				return &admissionv1.AdmissionResponse{
					Result: &metav1.Status{
						Message: extractErr.Error(),
						Reason:  metav1.StatusReasonBadRequest,
					},
				}
			}
		default:
			msg := fmt.Sprintf(
				"expected resource to be one of %v, got %s",
				[]metav1.GroupVersionResource{
					resourceClaimResourceV1, resourceClaimResourceV1Beta1, resourceClaimResourceV1Beta2,
					resourceClaimTemplateResourceV1, resourceClaimTemplateResourceV1Beta1, resourceClaimTemplateResourceV1Beta2,
				},
				ar.Request.Resource,
			)
			logger.Warn("Rejected request for unexpected resource")
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: msg,
					Reason:  metav1.StatusReasonBadRequest,
				},
			}
		}

		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}
}
