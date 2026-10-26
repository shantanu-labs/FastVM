package unit

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/shantanuroy/airbox/pkg/sandbox"
	"github.com/sirupsen/logrus"
)

func TestOpenHandsStatefulLifecycleAndMCTSBranching(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr, err := sandbox.NewManager(sandbox.Config{
		PoolSize:   10,
		OverlayDir: t.TempDir(),
	}, log)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	server := api.NewServer(mgr, log)
	ts := httptest.NewServer(server.Routes())
	defer ts.Close()

	execReq := func(sbxID, lang, code string) api.ExecuteResponse {
		t.Helper()
		body, _ := json.Marshal(api.ExecuteRequest{
			SandboxID: sbxID,
			Language:  lang,
			Code:      code,
			TimeoutMs: 5000,
		})
		resp, err := http.Post(ts.URL+"/v1/execute", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /v1/execute failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status %d", resp.StatusCode)
		}
		var out api.ExecuteResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		return out
	}

	// 1. Initialize stateful Python session in pinned OpenHands sandbox
	res1 := execReq("oh-session-1", "python", "step = 4; plan = ['build', 'test']")
	if res1.ExitCode != 0 {
		t.Fatalf("step 1 failed: %+v", res1)
	}

	// 2. Interleave a Bash command on the SAME pinned sandbox ID
	resBash := execReq("oh-session-1", "bash", "echo -n 'bash-ok'")
	if resBash.ExitCode != 0 || resBash.Stdout != "bash-ok" {
		t.Fatalf("expected bash-ok on pinned sandbox, got %q (exit %d, stderr %q)", resBash.Stdout, resBash.ExitCode, resBash.Stderr)
	}

	// 3. Branch parent sandbox into an MCTS child branch
	branchBody, _ := json.Marshal(api.BranchRequest{NewSandboxID: "oh-mcts-branch-1"})
	bResp, err := http.Post(ts.URL+"/v1/sandboxes/oh-session-1/branch", "application/json", bytes.NewReader(branchBody))
	if err != nil {
		t.Fatalf("branch request failed: %v", err)
	}
	defer bResp.Body.Close()
	if bResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on branch, got %d", bResp.StatusCode)
	}

	// 4. Mutate state in child branch
	resBranch := execReq("oh-mcts-branch-1", "python", "step += 10; plan.append('deploy'); print(step, len(plan))")
	if strings.TrimSpace(resBranch.Stdout) != "14 3" {
		t.Fatalf("expected branched state '14 3', got %q (stderr: %s)", resBranch.Stdout, resBranch.Stderr)
	}

	// 5. Verify parent sandbox state was not mutated by child branch
	resParent := execReq("oh-session-1", "python", "print(step, len(plan))")
	if strings.TrimSpace(resParent.Stdout) != "4 2" {
		t.Fatalf("expected parent state '4 2' to remain isolated, got %q", resParent.Stdout)
	}

	// 6. Terminate sandboxes via DELETE /v1/sandboxes/{id}
	for _, id := range []string{"oh-mcts-branch-1", "oh-session-1"} {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sandboxes/"+id, nil)
		delResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /v1/sandboxes/%s failed: %v", id, err)
		}
		delResp.Body.Close()
		if delResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on DELETE /v1/sandboxes/%s, got %d", id, delResp.StatusCode)
		}
	}
}

func TestOpenHandsPythonAdapterAndMCTSDemo(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr, err := sandbox.NewManager(sandbox.Config{
		PoolSize:   10,
		OverlayDir: t.TempDir(),
	}, log)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	server := api.NewServer(mgr, log)
	ts := httptest.NewServer(server.Routes())
	defer ts.Close()

	demoScript := filepath.Join("..", "..", "integrations", "openhands", "examples", "mcts_branching_demo.py")
	cmd := exec.Command("python3", demoScript, "--url", ts.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("OpenHands MCTS demo script failed: %v\nOutput:\n%s", err, string(out))
	}

	outputStr := string(out)
	if !strings.Contains(outputStr, "[oh-branch-patch-2] -> PASS") {
		t.Errorf("expected oh-branch-patch-2 to PASS in demo output, got:\n%s", outputStr)
	}
	if !strings.Contains(outputStr, "Winning MCTS branch selected: oh-branch-patch-2") {
		t.Errorf("expected winning branch oh-branch-patch-2, got:\n%s", outputStr)
	}
}
