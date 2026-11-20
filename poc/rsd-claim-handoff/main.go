// Hardware feasibility probe for one explicitly enrolled Pod on a disposable
// node. The kubelet driver's ordinary claim preparation supplies the CDI spec.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const driver = "npu.rebellions.ai"
const groupLock = "/run/rbln/.dra-handoff-poc.lock"

type device struct {
	Name  string      `json:"device"`
	Group json.Number `json:"group_id"`
	Index json.Number `json:"npu"`
	PCI   struct {
		Address string `json:"bus_id"`
	} `json:"pci"`
}

type probe struct {
	namespace, pod, uid, claim, claimUID, node, peer, binary string
	client                                                   kubernetes.Interface
	mu                                                       sync.Mutex
	group                                                    string
	deviceNumber                                             uint64
	prior                                                    []string
	initID                                                   string
	initStopped                                              bool
}

func (p *probe) Synchronize(_ context.Context, _ []*api.PodSandbox, _ []*api.Container) ([]*api.ContainerUpdate, error) {
	return nil, os.WriteFile("/tmp/nri-ready", []byte("ready"), 0o600)
}

func (p *probe) StopContainer(_ context.Context, pod *api.PodSandbox, c *api.Container) ([]*api.ContainerUpdate, error) {
	if pod.GetUid() == p.uid && c.GetName() == "init-npu" {
		p.mu.Lock()
		if c.GetId() == p.initID {
			p.initStopped = true
		}
		p.mu.Unlock()
	}
	return nil, nil
}

func (p *probe) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if pod.GetUid() != p.uid || pod.GetNamespace() != p.namespace || pod.GetName() != p.pod {
		if pod.GetName() == p.peer && pod.GetNamespace() == p.namespace {
			log.Printf("ignored different Pod UID %s", pod.GetUid())
		}
		return nil, nil, nil
	}
	name := ctr.GetName()
	if name != "init-npu" && name != "ctr0" {
		return nil, nil, errors.New("unexpected enrolled container")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.prepare(ctx, name)
	if err != nil {
		log.Printf("refusing %s for Pod UID %s: %v", name, p.uid, err)
		return nil, nil, err
	}
	if name == "init-npu" {
		p.initID = ctr.GetId()
		p.initStopped = false
	}
	return nil, nil, nil
}

func (p *probe) command(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("SMI %q: %w: %.600s", args, err, out)
	}
	return out, nil
}
func (p *probe) read(ctx context.Context) ([]device, error) {
	raw, err := p.command(ctx, "-g", "-j")
	if err != nil {
		return nil, err
	}
	var d struct {
		Devices []device `json:"devices"`
	}
	if err = json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range d.Devices {
		normalized, err := system.NormalizePCIAddress(d.Devices[i].PCI.Address)
		if err != nil || seen[normalized] {
			return nil, errors.New("invalid SMI identity")
		}
		d.Devices[i].PCI.Address = normalized
		seen[normalized] = true
	}
	return d.Devices, nil
}
func members(inventory []device, group string) []string {
	var m []string
	for _, d := range inventory {
		if d.Group.String() == group {
			m = append(m, d.PCI.Address)
		}
	}
	slices.Sort(m)
	return m
}
func rdev(group string) (uint64, error) {
	id, err := strconv.ParseUint(group, 10, 32)
	if err != nil || id == 0 {
		return 0, errors.New("invalid private group ID")
	}
	path := fmt.Sprintf("/dev/rsd%d", id)
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return 0, errors.New("RSD path is not a character device")
	}
	return uint64(st.Rdev), nil
}

func (p *probe) create(ctx context.Context, group string, wanted []string) error {
	inv, err := p.read(ctx)
	if err != nil {
		return err
	}
	if len(members(inv, group)) != 0 {
		return errors.New("claim RSD slot is still occupied")
	}
	help, err := p.command(ctx, "group", "--help")
	if err != nil {
		return err
	}
	contract := strings.ToLower(string(help))
	usePCI := strings.Contains(contract, "pci address")
	if !usePCI && !strings.Contains(contract, "attach npu ids") {
		return errors.New("unsupported SMI selector syntax")
	}
	var selectors []string
	for _, addr := range wanted {
		found := false
		for _, d := range inv {
			if d.PCI.Address != addr {
				continue
			}
			if d.Group.String() != "0" {
				return errors.New("claim member belongs to another RSD slot")
			}
			v := addr
			if !usePCI {
				i, err := d.Index.Int64()
				if err != nil || i < 0 {
					return errors.New("invalid default-group index")
				}
				v = strconv.FormatInt(i, 10)
			}
			selectors = append(selectors, v)
			found = true
			break
		}
		if !found {
			return fmt.Errorf("claim member disappeared: %s", addr)
		}
	}
	_, err = p.command(ctx, "group", "-c", group, "-a", strings.Join(selectors, ","))
	return err
}

// A failed attempt can be retried. Never remove a group outside the enrolled
// claim, the default group, or a slot that has changed ownership/membership.
func (p *probe) prepare(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, p.pod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(pod.UID) != p.uid || pod.Spec.NodeName != p.node || pod.DeletionTimestamp != nil || pod.Spec.RestartPolicy != corev1.RestartPolicyNever ||
		len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers) != 1 || len(pod.Spec.EphemeralContainers) != 0 || pod.Spec.InitContainers[0].RestartPolicy != nil ||
		pod.Spec.InitContainers[0].Name != "init-npu" || pod.Spec.Containers[0].Name != "ctr0" ||
		len(pod.Spec.ResourceClaims) != 1 || pod.Spec.ResourceClaims[0].ResourceClaimName == nil || *pod.Spec.ResourceClaims[0].ResourceClaimName != p.claim {
		return errors.New("enrolled Pod topology changed")
	}
	c := pod.Spec.InitContainers[0]
	if name == "ctr0" {
		finished := p.initStopped
		for _, s := range pod.Status.InitContainerStatuses {
			if s.Name == "init-npu" && s.State.Terminated != nil && s.State.Terminated.ExitCode == 0 {
				finished = true
			}
		}
		if !finished {
			return errors.New("init has not completed")
		}
		c = pod.Spec.Containers[0]
	}
	if len(c.Resources.Claims) != 1 || c.Resources.Claims[0].Name != pod.Spec.ResourceClaims[0].Name {
		return errors.New("unexpected claim reference")
	}
	claim, err := p.client.ResourceV1().ResourceClaims(p.namespace).Get(ctx, p.claim, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(claim.UID) != p.claimUID || claim.DeletionTimestamp != nil || claim.Status.Allocation == nil {
		return errors.New("claim identity or allocation unavailable")
	}
	reserved := false
	for _, r := range claim.Status.ReservedFor {
		if string(r.UID) == p.uid && r.Resource == "pods" && r.Name == p.pod {
			reserved = true
		}
	}
	if !reserved {
		return errors.New("claim is not reserved for this Pod UID")
	}
	f, err := os.OpenFile(groupLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	inv, err := p.read(ctx)
	if err != nil {
		return err
	}
	var full, want []string
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != driver || r.Pool != p.node {
			return errors.New("unexpected allocation driver or pool")
		}
		var addr string
		for _, d := range inv {
			if d.Name == r.Device {
				addr = d.PCI.Address
				break
			}
		}
		if addr == "" || slices.Contains(full, addr) {
			return fmt.Errorf("unknown or duplicate native allocation %s", r.Device)
		}
		full = append(full, addr)
		if c.Resources.Claims[0].Request == "" || c.Resources.Claims[0].Request == r.Request {
			want = append(want, addr)
		}
	}
	slices.Sort(full)
	slices.Sort(want)
	if len(full) < 1 || len(full) > 2 || len(want) == 0 {
		return errors.New("unsupported test claim size or request selection")
	}
	if p.group == "" {
		for _, d := range inv {
			if d.PCI.Address == full[0] {
				p.group = d.Group.String()
				break
			}
		}
		if p.group == "0" || !slices.Equal(members(inv, p.group), full) {
			p.group = ""
			return errors.New("initial RSD group does not match this claim")
		}
		p.deviceNumber, err = rdev(p.group)
		if err != nil {
			p.group = ""
			return err
		}
		p.prior = full
	}
	current := members(inv, p.group)
	if !slices.Equal(current, p.prior) && !slices.Equal(current, want) {
		return errors.New("unexpected change to private RSD slot")
	}
	for _, d := range inv {
		if slices.Contains(want, d.PCI.Address) && d.Group.String() != "0" && d.Group.String() != p.group {
			return errors.New("a requested function belongs to another group")
		}
	}
	if !slices.Equal(current, want) {
		if len(claim.Status.ReservedFor) != 1 {
			return errors.New("another consumer is reserved; retaining existing group")
		}
		if _, err = p.command(ctx, "group", "-d", p.group); err != nil {
			return err
		}
		if err = p.create(ctx, p.group, want); err != nil {
			// Restore only when the exact slot is free and all former members are free.
			restoreErr := p.create(ctx, p.group, current)
			return errors.Join(err, restoreErr)
		}
	}
	readback, err := p.read(ctx)
	if err != nil {
		return err
	}
	if !slices.Equal(members(readback, p.group), want) {
		return errors.New("RSD membership readback failed")
	}
	actual, err := rdev(p.group)
	if err != nil {
		return err
	}
	if actual != p.deviceNumber {
		return errors.New("RSD device number changed after OCI path resolution")
	}
	p.prior = want
	log.Printf("verified PodUID=%s phase=%s slot=%s pci=%v rdev=%d", p.uid, name, p.group, want, actual)
	return nil
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	p := &probe{namespace: os.Getenv("TEST_NS"), pod: os.Getenv("TARGET_POD"), uid: os.Getenv("TARGET_POD_UID"), claim: os.Getenv("TARGET_CLAIM"), claimUID: os.Getenv("TARGET_CLAIM_UID"), peer: os.Getenv("PEER_POD"), node: os.Getenv("TARGET_NODE")}
	if p.uid == "" || p.claimUID == "" || p.pod == "" || p.namespace == "" || p.claim == "" || p.node == "" {
		log.Fatal("missing explicit Pod and claim enrollment")
	}
	for _, path := range []string{"/usr/bin/rbln-smi", "/run/rbln/driver/usr/bin/rbln-smi", "/host/usr/bin/rbln-smi"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			p.binary = path
			break
		}
	}
	if p.binary == "" {
		log.Fatal("SMI unavailable")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	p.client, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	s, err := stub.New(p, stub.WithPluginName("rbln-claim-handoff-poc"), stub.WithPluginIdx("94"))
	if err != nil {
		log.Fatal(err)
	}
	if err = s.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
