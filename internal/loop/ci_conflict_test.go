package loop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/target"
)

// conflictAwareGH stands in for GitHub the way it treats a conflicting pull
// request: the checks and Actions runs of a head exist only while that head
// merges cleanly into main, and `mergeable` says which it is. The first watch
// reports the red check that starts the CI fix; every later one passes on the
// external check alone, as `gh pr checks` does right after a push.
func conflictAwareGH(t *testing.T, origin string) string {
	t.Helper()
	state := t.TempDir()
	return writeTool(t, "gh", `
origin='`+origin+`'
state='`+state+`'
conflicts() { ! git -C "$origin" merge-tree --write-tree refs/heads/main "$1" >/dev/null 2>&1; }
head=$(git -C "$origin" rev-parse refs/heads/feature)
case "$1 $2" in
  "pr checks")
    case "$*" in
      *--json*) echo '[{"name":"ci-gate","bucket":"fail","workflow":"CI","link":"","description":""}]'; exit 1 ;;
    esac
    if ! test -f "$state/red"; then : > "$state/red"; printf 'ci-gate\tfail\n'; exit 1; fi
    printf 'secret-scan\tpass\n'; exit 0 ;;
  "pr view")
    case "$*" in
      *mergeable*)
        m=MERGEABLE; if conflicts "$head"; then m=CONFLICTING; fi
        printf '{"baseRefName":"main","headRefOid":"%s","state":"OPEN","mergeable":"%s"}\n' "$head" "$m" ;;
      *) echo "$head" ;;
    esac
    exit 0 ;;
esac
test "$1" = api || { echo "unexpected: $*" >&2; exit 1; }
for a in "$@"; do url=$a; done
case "$url" in
  *check-runs*)
    sha=${url#*commits/}; sha=${sha%%/*}
    if conflicts "$sha"; then echo '{"check_runs":[]}'
    else echo '{"check_runs":[{"id":1,"name":"ci-gate","status":"completed","conclusion":"success"}]}'; fi ;;
  */status\?*) echo '{"statuses":[{"context":"secret-scan","state":"success"}]}' ;;
  *actions/runs*)
    sha=${url#*head_sha=}; sha=${sha%%&*}
    if conflicts "$sha"; then echo '{"workflow_runs":[]}'
    else echo '{"workflow_runs":[{"id":11,"name":"main","workflow_id":3,"event":"pull_request","status":"completed","conclusion":"success","html_url":"https://github.com/acme/api/actions/runs/11"}]}'; fi ;;
  *actions/workflows*) echo '{"workflows":[{"path":".github/workflows/ci.yml","state":"active"}]}' ;;
  *rules/branches/*) echo '[]' ;;
  *branches/*) echo '{"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["ci-gate"]}}}' ;;
  *) echo "unexpected api call: $*" >&2; exit 1 ;;
esac`)
}

// ciConflictRun is a pull request whose CI is red while main already carries
// its own repair of the same file. The branch merges cleanly until the fake
// fix session commits a different repair; resumed inside the merge that
// follows, the same session resolves it.
func ciConflictRun(t *testing.T, f *baseUpdateRepo) *run {
	t.Helper()
	f.write("shared.txt", "base repair\n")
	f.git(f.clone, "add", "shared.txt")
	f.git(f.clone, "commit", "-q", "-m", "repair the gate on main")
	f.git(f.clone, "push", "-q", "origin", "main")

	codexBin := writeTool(t, "codex", `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  printf 'base repair\nbranch repair\n' > shared.txt
  git add shared.txt
  git commit -q -m "Merge main into feature"
  printf 'merged and resolved\n' > "$out"
else
  printf 'branch repair\n' > shared.txt
  git add shared.txt
  git commit -q -m "repair the gate on the branch"
  printf 'repaired the check\n' > "$out"
fi`)

	root := t.TempDir()
	client := gh.New(conflictAwareGH(t, f.origin))
	client.Backoff = time.Millisecond
	r := f.run(nil, 0)
	r.p.GH = client
	r.target = target.Target{}
	r.pr = gh.FullPR{Number: 42, BaseRefName: "main"}
	r.o.Repo = "acme/api"
	r.o.BaseUpdateOnConflict = true
	r.o.MaxCIFixes = 1
	r.o.FixTimeout = time.Minute
	r.timing = fastCIWait(200 * time.Millisecond)
	r.sessionID = "session-under-test"
	r.fixer = codex.Options{Bin: codexBin}
	r.root = root
	r.logDir = filepath.Join(root, "logs")
	r.msgDir = filepath.Join(root, "messages")
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (f *baseUpdateRepo) contains(ancestor, head string) bool {
	return exec.Command("git", "-C", f.origin, "merge-base", "--is-ancestor", ancestor, head).Run() == nil
}

// A CI fix can collide with a repair the base made in the meantime. GitHub
// starts no pull_request workflow for a head that conflicts with its base, so
// without a base update the required check of the pushed fix never reports
// and the run ends on that instead of on green.
func TestCIFixThatConflictsWithTheBaseIsMergedBeforeItsCIWait(t *testing.T) {
	f := newBaseUpdateRepo(t)
	r := ciConflictRun(t, f)

	if err := r.ensureCIGreen(); err != nil {
		t.Fatalf("ensureCIGreen = %v, want the conflict resolved and CI green", err)
	}
	if r.conflictFixes != 1 {
		t.Fatalf("conflict sessions = %d, want 1", r.conflictFixes)
	}
	pushed := f.git(f.origin, "rev-parse", "refs/heads/feature")
	if pushed != r.headSHA {
		t.Fatalf("origin/feature = %s, want the pinned head %s", pushed, r.headSHA)
	}
	if base := f.git(f.origin, "rev-parse", "refs/heads/main"); !f.contains(base, pushed) {
		t.Fatalf("the green head %s does not contain main at %s", pushed, base)
	}
	if content, err := os.ReadFile(filepath.Join(f.worktree, "shared.txt")); err != nil ||
		string(content) != "base repair\nbranch repair\n" {
		t.Fatalf("shared.txt = %q, %v", content, err)
	}
	if r.prog.CI != CIGreen {
		t.Fatalf("CI = %q, want %q", r.prog.CI, CIGreen)
	}
	if r.review != nil {
		t.Fatal("a CI wait that had no review beside it started one")
	}
}

// heldReviewer announces every review it is asked for and holds it until its
// context ends, the way a review outlasts a short CI wait.
type heldReviewer struct{ started chan struct{} }

func (f *heldReviewer) Run(ctx context.Context, _ review.Options) (*review.Result, error) {
	f.started <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A CI fix cancels the review of the outgoing head. The wait for the repaired
// head's checks suits a review as well as the first wait did: a run that
// starts it only once CI is green queues a whole review behind every fix.
func TestCIFixRestartsTheReviewBesideItsCIWait(t *testing.T) {
	f := newBaseUpdateRepo(t)
	r := ciConflictRun(t, f)
	reviewer := &heldReviewer{started: make(chan struct{}, 4)}
	r.p.Review = reviewer
	r.startReviewWith(2, "")
	defer r.killReview()

	if err := r.ensureCIGreen(); err != nil {
		t.Fatalf("ensureCIGreen = %v, want CI green after the fix", err)
	}
	if r.review == nil {
		t.Fatal("the CI wait after the fix ran without a review of the repaired head")
	}
	if r.review.round != 2 {
		t.Fatalf("restarted review is for round %d, want the cancelled round 2", r.review.round)
	}
	for _, which := range []string{"the review of the outgoing head", "the review of the repaired head"} {
		select {
		case <-reviewer.started:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never started", which)
		}
	}
}

// An offline run defers its base updates to the final push, but a CI fix is
// already past that push: the merge that resolves its conflict has to reach
// the remote before the wait can see checks for it.
func TestOfflineCIFixConflictIsMergedAndPushed(t *testing.T) {
	f := newBaseUpdateRepo(t)
	r := ciConflictRun(t, f)
	r.o.Offline = true

	if err := r.ensureCIGreen(); err != nil {
		t.Fatalf("ensureCIGreen = %v, want the conflict resolved and CI green", err)
	}
	pushed := f.git(f.origin, "rev-parse", "refs/heads/feature")
	if base := f.git(f.origin, "rev-parse", "refs/heads/main"); !f.contains(base, pushed) {
		t.Fatalf("the pushed head %s does not contain main at %s", pushed, base)
	}
	if pushed != r.headSHA || pushed != f.git(f.worktree, "rev-parse", "HEAD") {
		t.Fatalf("origin/feature = %s, pinned head = %s; want the worktree head", pushed, r.headSHA)
	}
}

// The wait after the terminal suggestion round has no review behind it, so it
// must not merge the base. It stops instead, and names the conflict rather
// than a check that did not report.
func TestFinalCIWaitNamesTheConflictInsteadOfMergingTheBase(t *testing.T) {
	f := newBaseUpdateRepo(t)
	r := ciConflictRun(t, f)

	err := r.ensureFinalCIGreen()
	if !errors.Is(err, ErrCIUnconfirmed) {
		t.Fatalf("ensureFinalCIGreen = %v, want ErrCIUnconfirmed", err)
	}
	if !strings.Contains(err.Error(), "conflicts with main") {
		t.Fatalf("error = %q, want it to name the conflict with main", err)
	}
	if r.conflictFixes != 0 {
		t.Fatal("the final CI wait started a conflict session")
	}
	pushed := f.git(f.origin, "rev-parse", "refs/heads/feature")
	if base := f.git(f.origin, "rev-parse", "refs/heads/main"); f.contains(base, pushed) {
		t.Fatalf("the final CI wait merged main into %s", pushed)
	}
}

// With base updates switched off nothing resolves the conflict, and the stop
// still has to say what GitHub is waiting for.
func TestCIWaitNamesTheConflictWhenBaseUpdatesAreOff(t *testing.T) {
	f := newBaseUpdateRepo(t)
	r := ciConflictRun(t, f)
	r.o.ResolveConflicts = false

	err := r.ensureCIGreen()
	if !errors.Is(err, ErrCIUnconfirmed) || !strings.Contains(err.Error(), "conflicts with main") {
		t.Fatalf("ensureCIGreen = %v, want ErrCIUnconfirmed naming the conflict with main", err)
	}
}
