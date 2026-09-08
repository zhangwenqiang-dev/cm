package connectmac

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPListProfiles(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_list_profiles", map[string]interface{}{})
	if !strings.Contains(out, "xcode-vnc") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "PROFILE") || !strings.Contains(out, "DESCRIPTION") {
		t.Fatalf("output missing table header = %q", out)
	}
	if len(runner.rsync) != 0 {
		t.Fatal("list must not run rsync")
	}
}

func TestMCPConfigLoadErrorReturnsToolResult(t *testing.T) {
	app, _, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, filepath.Join(t.TempDir(), "missing.yaml"), "cm_list_profiles", map[string]interface{}{})
	if !strings.Contains(out, "open") || data["ok"] != false || data["kind"] != "config" {
		t.Fatalf("out=%q structuredContent=%#v", out, data)
	}
}

func TestMCPGuideDoesNotRequireConfig(t *testing.T) {
	app, _, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, filepath.Join(t.TempDir(), "missing.yaml"), "cm_mcp_guide", map[string]interface{}{})
	if !strings.Contains(out, "ConnectMac MCP guide") || !strings.Contains(out, "confirm=true") {
		t.Fatalf("output = %q", out)
	}
	if data["primary_identity"] == "" || data["safe_destroy"] == "" {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPCheckAsksForMissingIdentityFile(t *testing.T) {
	app, config, _ := mcpTestAppWithConfig(t, strings.ReplaceAll(strings.ReplaceAll(sampleConfig,
		"  identity_file: ~/.ssh/default.pem\n", ""),
		"    identity_file: ~/.ssh/example.pem\n", ""))
	out, data := runMCPCallResult(t, app, config, "cm_check_profile", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "missing required input: identity_file") {
		t.Fatalf("output = %q", out)
	}
	if data["ok"] != false || data["profile"] != "xcode-vnc" {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPCheckReturnsStructuredSuccess(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, config, "cm_check_profile", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "check passed") {
		t.Fatalf("output = %q", out)
	}
	if data["ok"] != true || data["profile"] != "xcode-vnc" || data["apple_email"] != "user@example.com" {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPProfileShow(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_profile_show", map[string]interface{}{
		"profile": "user@example.com",
	})
	if !strings.Contains(out, "xcode-vnc") || !strings.Contains(out, "user@example.com") {
		t.Fatalf("output = %q", out)
	}
}

func TestMCPProfileAddRequiresConfirm(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_profile_add", map[string]interface{}{
		"name":        "new-profile",
		"apple_email": "new@example.com",
	})
	if !strings.Contains(out, "Preview only") {
		t.Fatalf("output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(config), "profiles", "new-profile.yaml")); !os.IsNotExist(err) {
		t.Fatalf("profile should not be written during preview, err=%v", err)
	}
}

func TestMCPProfileAddConfirmWritesProfile(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_profile_add", map[string]interface{}{
		"name":        "new-profile",
		"apple_email": "new@example.com",
		"confirm":     true,
	})
	if !strings.Contains(out, "Created profile file") {
		t.Fatalf("output = %q", out)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(config), "profiles", "new-profile.yaml"))
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if !strings.Contains(string(data), "new@example.com") {
		t.Fatalf("profile file = %s", data)
	}
}

func TestMCPProfileRemoveBlocksWhenAWSResourcesExist(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "available"}}}}, nil
	}
	out := runMCPCall(t, app, config, "cm_profile_remove", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "still has AWS Mac resources") {
		t.Fatalf("output = %q", out)
	}
}

func TestMCPAWSCreateAsksForMissingCreator(t *testing.T) {
	configData := strings.ReplaceAll(sampleConfig, "  aws:\n    creator: Default Creator\n", "")
	configData = strings.ReplaceAll(configData, "      creator: Xiao Chen\n", "")
	app, config, _ := mcpTestAppWithConfig(t, configData)
	out, data := runMCPCallResult(t, app, config, "cm_aws_create_mac", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "missing required input: aws.creator") {
		t.Fatalf("output = %q", out)
	}
	if data["requires_user_input"] != true {
		t.Fatalf("data = %#v", data)
	}
}

func TestMCPAWSCreateDoesNotUseDefaultCreator(t *testing.T) {
	configData := strings.ReplaceAll(sampleConfig, "      creator: Xiao Chen\n", "")
	app, config, _ := mcpTestAppWithConfig(t, configData)
	out, data := runMCPCallResult(t, app, config, "cm_aws_create_mac", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "missing required input: aws.creator") {
		t.Fatalf("output = %q", out)
	}
	missing, _ := data["missing"].([]interface{})
	if len(missing) != 1 || missing[0] != "aws.creator" {
		t.Fatalf("data = %#v", data)
	}
}

func TestMCPAWSCreateAcceptsExplicitCreatorParameter(t *testing.T) {
	configData := strings.ReplaceAll(sampleConfig, "      creator: Xiao Chen\n", "")
	app, config, _ := mcpTestAppWithConfig(t, configData)
	out := runMCPCall(t, app, config, "cm_aws_create_mac", map[string]interface{}{
		"profile": "xcode-vnc",
		"creator": "Wang Henghui",
	})
	for _, want := range []string{"AWS Mac plan", "cm-creator=Wang Henghui", "Preview only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPGuideAndNext(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, filepath.Join(t.TempDir(), "missing.yaml"), "cm_guide", map[string]interface{}{
		"topic": "open",
	})
	if !strings.Contains(out, "ConnectMac open-Mac guide") || data["topic"] != "open" {
		t.Fatalf("guide out=%q data=%#v", out, data)
	}
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", SystemStatus: "ok", InstanceStatusCheck: "ok"}},
			ElasticIP: ElasticIP{InstanceID: "i-1", PublicIP: "54.1.2.3"},
		}}, nil
	}
	out, data = runMCPCallResult(t, app, config, "cm_next", map[string]interface{}{
		"profile": "user@example.com",
	})
	if !strings.Contains(out, "Decision: ready") || data["decision"] != "ready" || data["next"] != "cm start xcode-vnc" {
		t.Fatalf("next out=%q data=%#v", out, data)
	}
}

func TestMCPPushRequiresConfirm(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	localPath := filepath.Join(t.TempDir(), "file.txt")
	writeFile(t, localPath, "data")
	out, data := runMCPCallResult(t, app, config, "cm_push", map[string]interface{}{
		"profile":    "xcode-vnc",
		"local_path": localPath,
		"remote_dir": "~/Documents/",
	})
	if !strings.Contains(out, "Preview only") {
		t.Fatalf("output = %q", out)
	}
	if len(runner.rsync) != 0 {
		t.Fatal("preview must not run rsync")
	}
	if data["direction"] != "push" || data["confirmed"] != false || data["local_path"] != localPath || data["remote_dir"] != "~/Documents/" {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPPushConfirmRunsRsync(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	localPath := filepath.Join(t.TempDir(), "file.txt")
	writeFile(t, localPath, "data")
	out := runMCPCall(t, app, config, "cm_push", map[string]interface{}{
		"profile":    "xcode-vnc",
		"local_path": localPath,
		"remote_dir": "~/Documents/",
		"confirm":    true,
	})
	if !strings.Contains(out, "Executed") {
		t.Fatalf("output = %q", out)
	}
	if len(runner.rsync) == 0 {
		t.Fatal("confirm=true should run rsync")
	}
}

func TestMCPPushAcceptsSyncFilters(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	localPath := filepath.Join(t.TempDir(), "file.txt")
	writeFile(t, localPath, "data")
	out := runMCPCall(t, app, config, "cm_push", map[string]interface{}{
		"profile":    "xcode-vnc",
		"local_path": localPath,
		"remote_dir": "~/Documents/",
		"includes":   []interface{}{"Sources/***"},
		"excludes":   []interface{}{"DerivedData"},
		"confirm":    true,
	})
	if !strings.Contains(out, "Executed") {
		t.Fatalf("output = %q", out)
	}
	for _, want := range []string{"--include", "Sources/***", "--exclude", "xcuserdata", "--exclude", "DerivedData", "--exclude", "*"} {
		if !containsString(runner.rsync, want) {
			t.Fatalf("rsync args missing %q = %#v", want, runner.rsync)
		}
	}
}

func TestMCPPullAcceptsSyncFilters(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, config, "cm_pull", map[string]interface{}{
		"profile":     "xcode-vnc",
		"remote_path": "~/Desktop/a.zip",
		"includes":    []interface{}{"*.zip"},
		"excludes":    []interface{}{"*.tmp"},
		"confirm":     true,
	})
	if !strings.Contains(out, "Executed") {
		t.Fatalf("output = %q", out)
	}
	if data["direction"] != "pull" || data["confirmed"] != true || data["remote_path"] != "~/Desktop/a.zip" {
		t.Fatalf("structuredContent = %#v", data)
	}
	for _, want := range []string{"--include", "*.zip", "--exclude", ".DS_Store", "--exclude", "*.tmp", "--exclude", "*"} {
		if !containsString(runner.rsync, want) {
			t.Fatalf("rsync args missing %q = %#v", want, runner.rsync)
		}
	}
}

func TestMCPSyncBlocksNonCurrentHostKeysBeforeRsync(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		args       map[string]interface{}
		configure  func(*fakeRunner)
		wantCode   string
		wantStatus HostKeyStatus
	}{
		{name: "push stale", tool: "cm_push", args: map[string]interface{}{
			"profile": "xcode-vnc", "remote_dir": "~/Documents/", "confirm": true,
		}, configure: func(r *fakeRunner) { r.knownHost = "mac-host.example.com ssh-ed25519 AAAAOLD\n" }, wantCode: "host_key_changed", wantStatus: HostKeyStale},
		{name: "pull missing", tool: "cm_pull", args: map[string]interface{}{
			"profile": "xcode-vnc", "remote_path": "~/Documents/file", "local_dir": ".", "confirm": true,
		}, configure: func(r *fakeRunner) { r.missingHostKey = true }, wantCode: "host_key_missing", wantStatus: HostKeyMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, config, runner := mcpTestApp(t)
			app.LogManager = NewLogManager(filepath.Join(t.TempDir(), "logs"))
			tc.configure(runner)
			if tc.tool == "cm_push" {
				localPath := filepath.Join(t.TempDir(), "file")
				writeFile(t, localPath, "data")
				tc.args["local_path"] = localPath
			}
			cfg, err := LoadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			server := MCPServer{App: app, ConfigPath: config}
			if tc.tool == "cm_push" {
				_, err = server.mcpPush(context.Background(), cfg, tc.args)
			} else {
				_, err = server.mcpPull(context.Background(), cfg, tc.args)
			}
			if err == nil || !strings.Contains(err.Error(), "host key") {
				t.Fatalf("MCP error=%v", err)
			}
			if len(runner.rsync) != 0 || runner.forgotHost != "" {
				t.Fatalf("blocked MCP sync invoked rsync or mutation: rsync=%v forgot=%q", runner.rsync, runner.forgotHost)
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 1 || entries[0].Action != "host-key.blocked" || entries[0].ErrorCode != tc.wantCode || entries[0].Status != string(tc.wantStatus) || entries[0].RequestID == "" {
				t.Fatalf("blocked entries=%+v", entries)
			}
		})
	}
}

func TestMCPForgetHostRequiresConfirm(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_forget_host", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "Preview only") {
		t.Fatalf("output = %q", out)
	}
	if runner.forgotHost != "" {
		t.Fatal("preview must not forget host")
	}
}

func TestMCPForgetHostConfirmRuns(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_forget_host", map[string]interface{}{
		"profile": "xcode-vnc",
		"confirm": true,
	})
	if !strings.Contains(out, "Executed") {
		t.Fatalf("output = %q", out)
	}
	if runner.forgotHost != "mac-host.example.com" {
		t.Fatalf("forgot host = %q", runner.forgotHost)
	}
}

func TestMCPAWSPlan(t *testing.T) {
	app, config, runner := mcpTestApp(t)
	out := runMCPCall(t, app, config, "cm_aws_plan", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "AWS Mac plan") || !strings.Contains(out, "xcode-user@example.com") {
		t.Fatalf("output = %q", out)
	}
	if len(runner.rsync) != 0 || runner.forgotHost != "" {
		t.Fatal("aws plan must not run side-effect commands")
	}
}

func TestMCPAWSCapacity(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{
			quotas: map[string]float64{"mac2.metal": 1},
			allHosts: []DedicatedHostStatus{
				{HostID: "h-1", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1"},
			},
			offerings: map[string][]string{"mac2.metal": {"usw2-az1"}},
		}, nil
	}
	out := runMCPCall(t, app, config, "cm_aws_capacity", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "AWS Mac capacity") || !strings.Contains(out, "Running Dedicated mac2 Hosts") {
		t.Fatalf("output = %q", out)
	}
}

func TestMCPFindProfileByAppleEmail(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, config, "cm_find_profile_by_apple", map[string]interface{}{
		"apple_email": "user@example.com",
	})
	if !strings.Contains(out, "Profile: xcode-vnc") {
		t.Fatalf("output = %q", out)
	}
	if data["found"] != true || data["profile"] != "xcode-vnc" || data["apple_email"] != "user@example.com" {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPFindProfileByAppleEmailAsksWhenMissing(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, config, "cm_find_profile_by_apple", map[string]interface{}{})
	if !strings.Contains(out, "Apple account email is required") || !strings.Contains(out, "xcode-vnc: user@example.com") {
		t.Fatalf("output = %q", out)
	}
	if data["ok"] != false || data["found"] != false {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPDashboardReturnsStructuredProfiles(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", SystemStatus: "ok", InstanceStatusCheck: "ok"}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", InstanceID: "i-1", PublicIP: "54.1.2.3"},
		}}, nil
	}
	out, data := runMCPCallResult(t, app, config, "cm_dashboard", map[string]interface{}{"aws": true})
	if !strings.Contains(out, "PROFILE") || data["ok"] != true || data["aws"] != true {
		t.Fatalf("out=%q structuredContent=%#v", out, data)
	}
	profiles, ok := data["profiles"].([]interface{})
	if !ok || len(profiles) != 1 {
		t.Fatalf("profiles = %#v", data["profiles"])
	}
	first, ok := profiles[0].(map[string]interface{})
	if !ok || first["profile"] != "xcode-vnc" || first["decision"] != "ready" || first["next"] != "cm start xcode-vnc" {
		t.Fatalf("profile item = %#v", profiles[0])
	}
}

func TestMCPMissingProfileReturnsToolResult(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, config, "cm_check_profile", map[string]interface{}{})
	if !strings.Contains(out, "profile is required") || data["ok"] != false || data["kind"] != "profile" {
		t.Fatalf("out=%q structuredContent=%#v", out, data)
	}
}

func TestMCPAWSOpenMacByEmailPreview(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	out, data := runMCPCallResult(t, app, config, "cm_aws_open_mac_by_email", map[string]interface{}{
		"apple_email": "user@example.com",
	})
	if !strings.Contains(out, "Resolved Apple account user@example.com -> profile xcode-vnc") || !strings.Contains(out, "AWS Mac open preview") {
		t.Fatalf("output = %q", out)
	}
	if data["profile"] != "xcode-vnc" || data["apple_email"] != "user@example.com" || data["decision"] != "create" || data["confirmed"] != false {
		t.Fatalf("structuredContent = %#v", data)
	}
	if data["next"] != "cm aws open xcode-vnc --confirm" {
		t.Fatalf("next = %#v", data["next"])
	}
}

func TestMCPAWSStatusReturnsStructuredDecision(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Hosts: []DedicatedHostStatus{{HostID: "h-empty", State: "available"}},
		}}, nil
	}
	out, data := runMCPCallResult(t, app, config, "cm_aws_status", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "AWS Mac status") {
		t.Fatalf("output = %q", out)
	}
	if data["decision"] != "launch-on-host" || data["host_id"] != "h-empty" {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPAWSDestroyByEmailPreviewReturnsStructuredSafety(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out, data := runMCPCallResult(t, app, config, "cm_aws_destroy_mac_by_email", map[string]interface{}{
		"apple_email": "user@example.com",
	})
	if !strings.Contains(out, "Preview only") || !strings.Contains(out, "retain the Elastic IP allocation") {
		t.Fatalf("output = %q", out)
	}
	if data["profile"] != "xcode-vnc" || data["apple_email"] != "user@example.com" || data["eip_retained"] != true || data["confirmed"] != false {
		t.Fatalf("structuredContent = %#v", data)
	}
}

func TestMCPAWSCreateFailureReportsReasonAndStops(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{
			eip:    ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
			runErr: errString("RunInstances rejected selected host"),
		}, nil
	}
	out := runMCPCall(t, app, config, "cm_aws_create_mac", map[string]interface{}{
		"profile": "xcode-vnc",
		"confirm": true,
	})
	for _, want := range []string{"AWS create failed", "RunInstances rejected selected host", "Stopped", "wait for explicit instructions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPAWSWaitReady(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			CallerIdentity: CallerIdentity{Account: "123456789012", ARN: "arn:aws:iam::123456789012:user/cm"},
			Instances: []InstanceStatus{{
				InstanceID:          "i-1",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
			}},
			ElasticIP: ElasticIP{InstanceID: "i-1"},
		}}, nil
	}
	app.AWSService.ReadyPollInterval = time.Millisecond
	app.AWSService.ReadyTimeout = time.Second
	out := runMCPCall(t, app, config, "cm_aws_wait_ready", map[string]interface{}{
		"profile": "xcode-vnc",
	})
	if !strings.Contains(out, "AWS Mac ready for profile xcode-vnc: true") {
		t.Fatalf("output = %q", out)
	}
}

func TestMCPToolsIncludesAWSHostWorkflow(t *testing.T) {
	app, config, _ := mcpTestApp(t)
	out := runMCPToolsList(t, app, config)
	for _, want := range []string{"cm_profile_add", "cm_profile_remove", "cm_doctor", "cm_dashboard", "cm_find_profile_by_apple", "cm_aws_capacity", "cm_aws_wait_ready", "cm_aws_adopt_host", "cm_aws_launch_on_host", "cm_aws_open_mac_by_email", "cm_aws_destroy_mac_by_email"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tools output missing %q: %q", want, out)
		}
	}
}

func mcpTestApp(t *testing.T) (App, string, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	return mcpTestAppWithPath(t, dir, config)
}

func runMCPToolsList(t *testing.T, app App, config string) string {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	if err != nil {
		t.Fatal(err)
	}
	var in bytes.Buffer
	if err := writeMCPMessage(&in, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	server := MCPServer{App: app, ConfigPath: config}
	if err := server.Serve(context.Background(), &in, &out); err != nil {
		t.Fatal(err)
	}
	return string(out.Bytes())
}

func mcpTestAppWithConfig(t *testing.T, configData string) (App, string, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	return mcpTestAppWithPath(t, dir, config)
}

func mcpTestAppWithPath(t *testing.T, dir, config string) (App, string, *fakeRunner) {
	t.Helper()
	runner := &fakeRunner{}
	app := App{
		Out:       &bytes.Buffer{},
		Err:       &bytes.Buffer{},
		Runner:    runner,
		Validator: NewValidatorForTest(nil),
		StateManager: StateManager{
			Dir:       filepath.Join(dir, "state"),
			IsRunning: func(pid int) bool { return pid == 55 },
		},
		AWSService: AWSService{Now: func() time.Time {
			return time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
		}},
	}
	return app, config, runner
}

func runMCPCall(t *testing.T, app App, config, tool string, args map[string]interface{}) string {
	t.Helper()
	text, _ := runMCPCallResult(t, app, config, tool, args)
	return text
}

func runMCPCallResult(t *testing.T, app App, config, tool string, args map[string]interface{}) (string, map[string]interface{}) {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      tool,
			"arguments": args,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var in bytes.Buffer
	if err := writeMCPMessage(&in, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	server := MCPServer{App: app, ConfigPath: config}
	if err := server.Serve(context.Background(), &in, &out); err != nil {
		t.Fatal(err)
	}
	return decodeMCPResult(t, out.Bytes())
}

func decodeMCPText(t *testing.T, data []byte) string {
	t.Helper()
	text, _ := decodeMCPResult(t, data)
	return text
}

func decodeMCPResult(t *testing.T, data []byte) (string, map[string]interface{}) {
	t.Helper()
	reader := bytes.NewReader(data)
	br := make([]byte, len(data))
	n, err := reader.Read(br)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(br[:n])
	_, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("invalid MCP response: %q", raw)
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
		Error *mcpError `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("mcp error: %v", resp.Error.Message)
	}
	if len(resp.Result.Content) == 0 {
		return "", resp.Result.StructuredContent
	}
	return resp.Result.Content[0].Text, resp.Result.StructuredContent
}

func TestMCPMessageRoundTrip(t *testing.T) {
	var out bytes.Buffer
	if err := writeMCPMessage(&out, map[string]string{"ok": "yes"}); err != nil {
		t.Fatal(err)
	}
	body, err := readMCPMessage(bufio.NewReader(bytes.NewBuffer(out.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "yes") {
		t.Fatalf("body = %s", body)
	}
}
