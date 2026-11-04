package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/utils/ptr"
)

// Evaluate the rendered chart with Kubernetes' actual DRA CEL environment.
// Classes must not mix PF/VF profiles or native/passthrough devices.
func TestDeviceClassesSelectOnlyTheirDeviceKindAndProfile(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm is required to render and validate DeviceClasses")
	}
	out, err := exec.Command("helm", "template", "test", filepath.Join("..", "..", "deployments", "helm", "k8s-dra-driver-npu"), "--namespace", "dra-test", "--api-versions", "resource.k8s.io/v1").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	classes := map[string]resourceapi.DeviceClass{}
	for {
		var class resourceapi.DeviceClass
		if err := decoder.Decode(&class); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if class.Kind == "DeviceClass" {
			classes[class.Name] = class
		}
	}
	const driver = "npu.rebellions.ai"
	types := []struct {
		name, kind string
		vfCount    int64
	}{
		{"npu.rebellions.ai", deviceTypeNpu, 0},
		{"npu-vf1.rebellions.ai", deviceTypeNpuVF, 1},
		{"npu-vf4.rebellions.ai", deviceTypeNpuVF, 4},
		{"vfio-npu.rebellions.ai", deviceTypeVfio, 0},
		{"vfio-npu-vf1.rebellions.ai", deviceTypeVfio, 1},
		{"vfio-npu-vf4.rebellions.ai", deviceTypeVfio, 4},
	}
	if len(classes) != len(types) {
		t.Fatalf("rendered %d DeviceClasses, want %d", len(classes), len(types))
	}
	for _, wantClass := range types {
		class, exists := classes[wantClass.name]
		if !exists || len(class.Spec.Selectors) != 1 || class.Spec.Selectors[0].CEL == nil {
			t.Fatalf("missing class selector: %s", wantClass.name)
		}
		compiled := dracel.GetCompiler(dracel.Features{}).CompileCELExpression(class.Spec.Selectors[0].CEL.Expression, dracel.Options{})
		if compiled.Error != nil {
			t.Fatal(compiled.Error)
		}
		for _, candidate := range types {
			attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{deviceTypeAttributeKey: {StringValue: ptr.To(candidate.kind)}}
			info := sriovInfo{numVFs: candidate.vfCount}
			if info.numVFs > 0 {
				info.parentPCIBusID = "0000:17:00.0"
			}
			info.setAttributes(attrs)
			matched, _, err := compiled.DeviceMatches(context.Background(), dracel.Device{Driver: driver, Attributes: attrs})
			if err != nil || matched != (wantClass.name == candidate.name) {
				t.Errorf("class %s on %s: matched=%v err=%v", wantClass.name, candidate.name, matched, err)
			}
		}
		// Older VFIO inventories have vf but no partitionMode. They may not
		// satisfy any profile class and must never fall through to the PF class.
		for _, vf := range []bool{false, true} {
			attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeVfio)}, vfAttributeKey: {BoolValue: ptr.To(vf)},
			}
			matched, _, err := compiled.DeviceMatches(context.Background(), dracel.Device{Driver: driver, Attributes: attrs})
			want := !vf && wantClass.name == "vfio-npu.rebellions.ai"
			if err != nil || matched != want {
				t.Errorf("legacy vf=%v matched %s: %v, %v", vf, wantClass.name, matched, err)
			}
		}
	}
}
