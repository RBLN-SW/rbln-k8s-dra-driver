package rblnsmi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
)

type RblnSmi struct {
	KMDVersion string   `json:"KMD_version"`
	Devices    []Device `json:"devices"`
}

type Device struct {
	GroupID string  `json:"group_id"`
	Npu     int     `json:"npu"`
	Name    string  `json:"name"`
	SID     string  `json:"sid"`
	UUID    string  `json:"uuid"`
	Device  string  `json:"device"`
	Status  string  `json:"status"`
	FWVer   string  `json:"fw_ver"`
	PCI     PCIInfo `json:"pci"`
	Memory  Memory  `json:"memory"`
}

type PCIInfo struct {
	Dev       string `json:"dev"`
	BusID     string `json:"bus_id"`
	NUMANode  string `json:"numa_node"`
	LinkSpeed string `json:"link_speed"`
	LinkWidth string `json:"link_width"`
}

type Memory struct {
	Used  string `json:"used"`
	Total string `json:"total"`
}

const (
	rblnSmiCommand     = "rbln-smi"
	rblnSmiGroupOption = "group"
	defaultExecTimeout = 5 * time.Second
	defaultGroupID     = "0"
	rsdDevice          = "/dev/rsd"
)

func getRblnSmiAbsolutePath() (string, error) {
	candidates := []string{
		"/usr/bin/rbln-smi",
		"/run/rbln/driver/usr/bin/rbln-smi",
		"/host/usr/bin/rbln-smi",
		"/host/driver/usr/bin/rbln-smi",
	}

	for _, p := range candidates {
		info, err := os.Stat(p)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}

	abs, err := exec.LookPath(rblnSmiCommand)
	if err != nil {
		return "", fmt.Errorf("cannot find %s command in predefined paths or PATH: %w", rblnSmiCommand, err)
	}
	return abs, nil
}

func runRblnSmiCommand(ctx context.Context, args ...string) ([]byte, error) {
	abs, err := getRblnSmiAbsolutePath()
	if err != nil {
		return nil, err
	}

	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, abs, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status, ok := ee.Sys().(syscall.WaitStatus)
			if ok {
				return nil, fmt.Errorf("command failed with exit code %d: %s", status.ExitStatus(), string(out))
			}
			return nil, fmt.Errorf("command failed with unknown exit status: %s", string(out))
		}
		return nil, fmt.Errorf("failed to start command: %w", err)
	}
	return out, nil
}

func getGroupedDeviceInfoFromRblnSmi(ctx context.Context) (*RblnSmi, error) {
	out, err := runRblnSmiCommand(ctx, "-g", "-j")
	if err != nil {
		return nil, err
	}
	var result RblnSmi
	if err = json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rbln-smi output: %w", err)
	}
	return &result, nil
}

func getDeviceInfoFromRblnSmi(ctx context.Context) (*RblnSmi, error) {
	out, err := runRblnSmiCommand(ctx, "-j")
	if err != nil {
		return nil, err
	}
	var result RblnSmi
	if err = json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rbln-smi output: %w", err)
	}
	return &result, nil
}

// GetDeviceInfo retrieves device information from rbln-smi.
func GetDeviceInfo(ctx context.Context) (*RblnSmi, error) {
	return getDeviceInfoFromRblnSmi(ctx)
}

func collectRsdGroupIDs(smiInfo *RblnSmi, devices []string) []string {
	var filterdDevices []Device

	if len(devices) == 0 {
		filterdDevices = smiInfo.Devices
	} else {
		deviceSet := make(map[string]struct{}, len(devices))
		for _, device := range devices {
			deviceSet[device] = struct{}{}
		}

		for _, v := range smiInfo.Devices {
			if _, ok := deviceSet[v.PCI.BusID]; ok {
				filterdDevices = append(filterdDevices, v)
			}
		}
	}

	seen := make(map[string]struct{})
	groupIDs := make([]string, 0, len(filterdDevices))
	for _, fd := range filterdDevices {
		if fd.GroupID == defaultGroupID {
			continue
		}
		if _, exists := seen[fd.GroupID]; !exists {
			seen[fd.GroupID] = struct{}{}
			groupIDs = append(groupIDs, fd.GroupID)
		}
	}
	return groupIDs
}

func getRsdGroupIDs(smiInfo *RblnSmi, devices []string) []string {
	return collectRsdGroupIDs(smiInfo, devices)
}

func getAllRsdGroupIDs(smiInfo *RblnSmi) []string {
	return collectRsdGroupIDs(smiInfo, nil)
}

func getNpuIDs(smiInfo *RblnSmi, devices []string) []int {
	var npuIDs []int

	for _, device := range devices {
		for _, v := range smiInfo.Devices {
			if v.PCI.BusID == device {
				npuIDs = append(npuIDs, v.Npu)
			}
		}
	}
	return npuIDs
}

func nextRsdGroupID(smiInfo *RblnSmi) (string, error) {
	groupIDs := getAllRsdGroupIDs(smiInfo)

	seen := make(map[int]struct{}, len(groupIDs))

	for _, s := range groupIDs {
		groupID, err := strconv.Atoi(s)
		if err != nil {
			return "", fmt.Errorf("invalid group id: %d", groupID)
		}
		seen[groupID] = struct{}{}
	}

	for i := 1; ; i++ {
		if _, ok := seen[i]; !ok {
			return strconv.Itoa(i), nil
		}
	}
}

func DestroyRsdGroup(ctx context.Context, deviceIDs []string) error {
	smiInfo, err := getGroupedDeviceInfoFromRblnSmi(ctx)
	if err != nil {
		return err
	}

	groupIDs := getRsdGroupIDs(smiInfo, deviceIDs)
	if len(groupIDs) > 0 {
		_, err = runRblnSmiCommand(ctx, rblnSmiGroupOption, "-d", strings.Join(groupIDs, ","))
		if err != nil {
			return err
		}
		glog.Infof("Destroyed RSD groups: %s", strings.Join(groupIDs, ","))
	}
	return nil
}

func CreateRsdGroup(ctx context.Context, devices []string) (groupID string, err error) {
	smiInfo, err := getGroupedDeviceInfoFromRblnSmi(ctx)
	if err != nil {
		return "", err
	}

	groupID, err = nextRsdGroupID(smiInfo)
	if err != nil {
		return "", err
	}

	npuIDs := getNpuIDs(smiInfo, devices)

	strIDs := make([]string, len(npuIDs))
	for i, id := range npuIDs {
		strIDs[i] = strconv.Itoa(id)
	}

	_, err = runRblnSmiCommand(
		ctx,
		rblnSmiGroupOption,
		"-c", groupID,
		"-a", strings.Join(strIDs, ","),
	)

	if err != nil {
		return "", err
	}

	glog.Infof("Created RSD group %s for devices: %s", groupID, strings.Join(devices, ","))
	return groupID, nil
}
