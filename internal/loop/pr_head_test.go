package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
)

// prHeadRun builds a run whose fake gh reports the PR head from a sequence
// (the n-th `gh pr view` gets the n-th line, later calls repeat the last one)
// and whose fake git reports originHead for the branch on origin.
func prHeadRun(t *testing.T, originHead string, prHeads ...string) *run {
	t.Helper()
	dir := t.TempDir()
	seq := filepath.Join(dir, "heads")
	if err := os.WriteFile(seq, []byte(strings.Join(prHeads, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ghBin := filepath.Join(dir, "gh")
	ghScript := `#!/bin/sh
set -eu
[ "$1 $2" = "pr view" ] || { echo "unexpected gh call: $*" >&2; exit 1; }
n=$(cat "` + dir + `/n" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "` + dir + `/n"
total=$(wc -l < "` + seq + `")
[ "$n" -le "$total" ] || n=$total
sed -n "${n}p" "` + seq + `"
`
	gitBin := filepath.Join(dir, "git")
	gitScript := `#!/bin/sh
set -eu
case "$1 $2" in
  "ls-remote origin") printf '` + originHead + `\trefs/heads/feature/crumb-tray\n' ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	for path, body := range map[string]string{ghBin: ghScript, gitBin: gitScript} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := gh.New(ghBin)
	c.Backoff = time.Millisecond
	return &run{
		p:      &Pipeline{GH: c, Git: git.New(gitBin)},
		o:      Options{Repo: "acme/api", RepoRoot: dir},
		ctx:    context.Background(),
		rep:    nopReporter{},
		pr:     gh.FullPR{Number: 42, BaseRefName: "main"},
		branch: "feature/crumb-tray",
		timing: timings{headPoll: time.Millisecond, headSettle: 50 * time.Millisecond},
	}
}

func TestAwaitPRHeadAcceptsAPRThatCatchesUp(t *testing.T) {
	r := prHeadRun(t, "new-sha", "old-sha", "old-sha", "new-sha")
	r.timing.headSettle = time.Minute
	if err := r.awaitPRHead("new-sha"); err != nil {
		t.Fatalf("awaitPRHead = %v, want nil", err)
	}
}

// GitHub sometimes drops a push event: the branch ref moves, the PR head and
// its CI stay on the old sha. The run must say so instead of reviewing the old
// head or blaming another pusher.
func TestAwaitPRHeadStopsOnAPRThatMissedThePush(t *testing.T) {
	r := prHeadRun(t, "new-sha", "old-sha")
	err := r.awaitPRHead("new-sha")
	if err == nil {
		t.Fatal("awaitPRHead = nil, want an error for a PR head that never caught up")
	}
	for _, want := range []string{"GitHub did not process the push", "old-sha", "new-sha", "git commit --allow-empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestWaitForPushedHeadNamesADroppedPushEvent(t *testing.T) {
	r := prHeadRun(t, "pushed-sha", "old-sha")
	err := r.waitForPushedHead("pushed-sha", time.Now().Add(20*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "GitHub did not process the push") {
		t.Fatalf("waitForPushedHead = %v, want the dropped push event named", err)
	}
	if strings.Contains(err.Error(), "someone else") {
		t.Errorf("error %q blames another pusher although origin has the pushed sha", err)
	}
}

func TestWaitForPushedHeadStillSuspectsAnotherPusher(t *testing.T) {
	r := prHeadRun(t, "foreign-sha", "foreign-sha")
	err := r.waitForPushedHead("pushed-sha", time.Now().Add(20*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "someone else pushing") {
		t.Fatalf("waitForPushedHead = %v, want the other-pusher error", err)
	}
}
