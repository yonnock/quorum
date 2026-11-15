package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/target"
)

// greenGH is a GitHub whose pull request 42 tracks origin/feature and whose
// one required check passes on every head. Comments it is asked to post land
// in the file it returns.
func greenGH(t *testing.T, origin string) (bin, comments string) {
	t.Helper()
	comments = filepath.Join(t.TempDir(), "comments")
	return writeTool(t, "gh", `
origin='`+origin+`'
head=$(git -C "$origin" rev-parse refs/heads/feature)
case "$1 $2" in
  "pr checks") printf 'ci-gate\tpass\n'; exit 0 ;;
  "pr comment") cat >> '`+comments+`'; echo https://github.com/acme/api/pull/42#issuecomment-1; exit 0 ;;
  "pr view")
    case "$*" in
      *mergeable*) printf '{"baseRefName":"main","headRefOid":"%s","state":"OPEN","mergeable":"MERGEABLE"}\n' "$head" ;;
      *) echo "$head" ;;
    esac
    exit 0 ;;
esac
test "$1" = api || { echo "unexpected: $*" >&2; exit 1; }
for a in "$@"; do url=$a; done
case "$url" in
  *check-runs*) echo '{"check_runs":[{"id":1,"name":"ci-gate","status":"completed","conclusion":"success"}]}' ;;
  */status\?*) echo '{"statuses":[]}' ;;
  *actions/runs*) echo '{"workflow_runs":[{"id":11,"name":"main","workflow_id":3,"event":"pull_request","status":"completed","conclusion":"success","html_url":"https://github.com/acme/api/actions/runs/11"}]}' ;;
  *actions/workflows*) echo '{"workflows":[{"path":".github/workflows/ci.yml","state":"active"}]}' ;;
  *rules/branches/*) echo '[]' ;;
  *branches/*) echo '{"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["ci-gate"]}}}' ;;
  *) echo "unexpected api call: $*" >&2; exit 1 ;;
esac`), comments
}

// suggestionRun is a pull request whose clean review left one Suggestion. Its
// fix session takes the Suggestion with one commit, and counts how often it
// was asked to do anything.
type suggestionRun struct {
	*run
	f        *baseUpdateRepo
	sessions string // one line per fix session started
	comments string // everything posted to the pull request
	clean    review.Findings
	reviewed []string // the heads the reviewer was asked for
	// later, when set, is what every review after the first finds.
	later *review.Findings
}

// newSuggestionRun builds the run. verdict is what the review of the
// suggestion round's head finds.
func newSuggestionRun(t *testing.T, verdict review.Findings) *suggestionRun {
	t.Helper()
	f := newBaseUpdateRepo(t)
	s := &suggestionRun{f: f, sessions: filepath.Join(t.TempDir(), "sessions")}
	codexBin := writeTool(t, "codex", `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
echo session >> '`+s.sessions+`'
if test -f suggestion.txt; then
  printf 'repaired\n' > repaired.txt
  git add repaired.txt
  git commit -q -m "repair what the push verification refused"
  printf 'done\n\nPR COMMENT:\nRepaired the push.\n' > "$out"
  exit 0
fi
printf 'suggested\n' > suggestion.txt
git add suggestion.txt
git commit -q -m "take the suggestion"
printf 'done\n\nPR COMMENT:\nTook the suggestion.\n' > "$out"`)

	ghBin, comments := greenGH(t, f.origin)
	s.comments = comments
	client := gh.New(ghBin)
	client.Backoff = time.Millisecond

	reviewer := reviewerFunc(func(_ context.Context, _ review.Options) (*review.Result, error) {
		head := f.git(f.worktree, "rev-parse", "HEAD")
		s.reviewed = append(s.reviewed, head)
		found := verdict
		if s.later != nil && len(s.reviewed) > 1 {
			found = *s.later
		}
		found.PR, found.HeadSHA = 42, head
		return &review.Result{Findings: found, CommentBody: "## Summary\n\nreview of " + head + "\n"}, nil
	})
	r := f.run(reviewer, 6)
	r.p.GH = client
	r.target = target.Target{}
	r.pr = gh.FullPR{Number: 42, BaseRefName: "main"}
	r.o.Repo = "acme/api"
	r.o.Post = true
	r.o.FixSuggestions = true
	r.o.VerifySuggestionRound = true
	r.o.MaxCIFixes = 1
	r.o.FixTimeout = time.Minute
	r.timing = fastCIWait(200 * time.Millisecond)
	r.sessionID = "session-under-test"
	r.fixer = codex.Options{Bin: codexBin}
	r.root = t.TempDir()
	r.logDir = filepath.Join(r.root, "logs")
	r.msgDir = filepath.Join(r.root, "messages")
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.run = r
	s.clean = review.Findings{PR: 42, Posted: true, HeadSHA: f.featureSHA, Suggestions: 1}
	return s
}

func (s *suggestionRun) sessionCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(s.sessions)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(data), "session")
}

// A run that merges has to end on a reviewed head. The suggestion round
// pushes commits after the clean review, so without a review of its own that
// head is one auto-merge must refuse, and every such run ends unmerged.
func TestSuggestionCommitsAreReviewedWhenTheRunMerges(t *testing.T) {
	s := newSuggestionRun(t, review.Findings{Posted: true, Suggestions: 2})
	res := &Result{Converged: true, Rounds: 1, LastFindings: s.clean}

	if err := s.terminalSuggestions(res, 1, s.clean, "## Summary\n\nfine", s.f.featureSHA); err != nil {
		t.Fatalf("terminalSuggestions = %v", err)
	}
	pushed := s.f.git(s.f.origin, "rev-parse", "refs/heads/feature")
	if pushed == s.f.featureSHA {
		t.Fatal("the suggestion round pushed nothing")
	}
	if len(s.reviewed) != 1 || s.reviewed[0] != pushed {
		t.Fatalf("reviewed heads = %v, want exactly the pushed suggestion head %s", s.reviewed, pushed)
	}
	if res.SuggestionCommits {
		t.Fatal("the reviewed suggestion commits still disqualify the head from auto-merge")
	}
	if res.LastFindings.HeadSHA != pushed || !res.LastFindings.MergeEligible() {
		t.Fatalf("final findings = %+v, want a merge-eligible review of %s", res.LastFindings, pushed)
	}
	if !res.Converged || res.Rounds != 2 {
		t.Fatalf("converged = %v after %d rounds, want converged after 2", res.Converged, res.Rounds)
	}
	// The second review still lists Suggestions. Nobody may act on them: a
	// suggestion round that feeds another one never ends.
	if got := s.sessionCount(t); got != 1 {
		t.Fatalf("fix sessions = %d, want the suggestion round alone", got)
	}
	if s.review != nil {
		t.Fatal("the run ended with a review still running")
	}
}

// The review of the suggestion commits only gates. A Blocker in it stops the
// run unmerged and unconverged; a fix round here would reopen the loop the
// suggestion round is meant to end.
func TestBlockerInTheSuggestionReviewStopsTheRunWithoutAFixRound(t *testing.T) {
	s := newSuggestionRun(t, review.Findings{Posted: true, Blockers: 1})
	res := &Result{Converged: true, Rounds: 1, LastFindings: s.clean}

	err := s.terminalSuggestions(res, 1, s.clean, "## Summary\n\nfine", s.f.featureSHA)
	if !errors.Is(err, ErrNotConverged) || !strings.Contains(err.Error(), "1 blocker") {
		t.Fatalf("terminalSuggestions = %v, want ErrNotConverged naming the blocker", err)
	}
	if res.Converged {
		t.Fatal("a run whose final review has a blocker reads as converged")
	}
	if !res.SuggestionCommits || res.LastFindings.MergeEligible() {
		t.Fatalf("suggestion commits = %v, findings = %+v; the head must stay unmergeable", res.SuggestionCommits, res.LastFindings)
	}
	if got := s.sessionCount(t); got != 1 {
		t.Fatalf("fix sessions = %d, want no fix round after the suggestion round", got)
	}
}

// Without a merge ahead the extra review buys nothing, and a draft or a run
// that posts nothing can never be merged.
func TestSuggestionCommitsStayUnreviewedWhenNothingMerges(t *testing.T) {
	for name, adjust := range map[string]func(*run){
		"auto-merge off": func(r *run) { r.o.VerifySuggestionRound = false },
		"draft":          func(r *run) { r.pr.IsDraft = true },
		"posting off":    func(r *run) { r.o.Post = false },
	} {
		s := newSuggestionRun(t, review.Findings{Posted: true})
		adjust(s.run)
		res := &Result{Converged: true, Rounds: 1, LastFindings: s.clean}

		if err := s.terminalSuggestions(res, 1, s.clean, "## Summary\n\nfine", s.f.featureSHA); err != nil {
			t.Fatalf("%s: terminalSuggestions = %v", name, err)
		}
		if len(s.reviewed) != 0 || s.review != nil {
			t.Errorf("%s: the suggestion commits were reviewed (%v)", name, s.reviewed)
		}
		if !res.SuggestionCommits || res.LastFindings.HeadSHA != s.f.featureSHA || res.Rounds != 1 {
			t.Errorf("%s: result = %+v, want the unreviewed suggestion head recorded", name, res)
		}
	}
}

// An offline run reviews the suggestion commits before its one push, so the
// comment it posts and the head it publishes belong to the same review.
func TestOfflineSuggestionCommitsAreReviewedBeforeThePush(t *testing.T) {
	s := newSuggestionRun(t, review.Findings{})
	s.o.Offline = true
	res := &Result{Rounds: 1, LastFindings: s.clean}

	settled, err := s.finalizeOffline(res, 1, s.clean, "## Summary\n\nstale")
	if err != nil || !settled {
		t.Fatalf("finalizeOffline = %v, %v; want the run settled", settled, err)
	}
	pushed := s.f.git(s.f.origin, "rev-parse", "refs/heads/feature")
	if len(s.reviewed) != 1 || s.reviewed[0] != pushed {
		t.Fatalf("reviewed heads = %v, want exactly the pushed head %s", s.reviewed, pushed)
	}
	if res.SuggestionCommits || !res.LastFindings.MergeEligible() || res.LastFindings.HeadSHA != pushed {
		t.Fatalf("suggestion commits = %v, findings = %+v; want a posted review of %s", res.SuggestionCommits, res.LastFindings, pushed)
	}
	posted, err := os.ReadFile(s.comments)
	if err != nil {
		t.Fatalf("nothing was posted: %v", err)
	}
	if !strings.Contains(string(posted), "review of "+pushed) || strings.Contains(string(posted), "stale") {
		t.Fatalf("posted comments do not carry the review of the pushed head:\n%s", posted)
	}
}

// rejectFirstPush installs a push verification that refuses the suggestion
// commit until the fix session has repaired it.
func (s *suggestionRun) rejectFirstPush(t *testing.T) {
	t.Helper()
	hook := filepath.Join(s.f.clone, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntest -f repaired.txt || { echo 'verify: repaired.txt is missing'; exit 1; }\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The repository's own push verification can refuse the reviewed head, and
// the session that repairs it commits past the review. The run must review
// that head too instead of ending on a review it cannot post: a run that was
// going to merge would end unmerged without saying why.
func TestOfflinePushRepairAfterTheSuggestionReviewIsReviewedAgain(t *testing.T) {
	s := newSuggestionRun(t, review.Findings{})
	s.o.Offline = true
	s.rejectFirstPush(t)
	res := &Result{Rounds: 1, LastFindings: s.clean}

	settled, err := s.finalizeOffline(res, 1, s.clean, "## Summary\n\nstale")
	if err != nil || !settled {
		t.Fatalf("finalizeOffline = %v, %v; want the run settled", settled, err)
	}
	pushed := s.f.git(s.f.origin, "rev-parse", "refs/heads/feature")
	if len(s.reviewed) != 2 || s.reviewed[0] == pushed || s.reviewed[1] != pushed {
		t.Fatalf("reviewed heads = %v, want the suggestion head and then the pushed repair %s", s.reviewed, pushed)
	}
	if res.LastFindings.HeadSHA != pushed || !res.LastFindings.MergeEligible() || res.SuggestionCommits {
		t.Fatalf("suggestion commits = %v, findings = %+v; want a posted review of %s", res.SuggestionCommits, res.LastFindings, pushed)
	}
	posted, _ := os.ReadFile(s.comments)
	if !strings.Contains(string(posted), "review of "+pushed) || strings.Contains(string(posted), "review of "+s.reviewed[0]) {
		t.Fatalf("posted comments do not carry exactly the review of the pushed head:\n%s", posted)
	}
}

// That second review gates like the first. A Blocker in it must not reach
// the loop, where it would start the fix round a suggestion round cannot have.
func TestOfflinePushRepairReviewWithABlockerStartsNoFixRound(t *testing.T) {
	s := newSuggestionRun(t, review.Findings{})
	s.o.Offline = true
	s.later = &review.Findings{Blockers: 1}
	s.rejectFirstPush(t)
	res := &Result{Rounds: 1, LastFindings: s.clean}

	settled, err := s.finalizeOffline(res, 1, s.clean, "## Summary\n\nstale")
	if settled || !errors.Is(err, ErrNotConverged) {
		t.Fatalf("finalizeOffline = %v, %v; want ErrNotConverged", settled, err)
	}
	if len(s.reviewed) != 2 {
		t.Fatalf("reviews = %d, want the suggestion head and the repaired head", len(s.reviewed))
	}
	if got := s.sessionCount(t); got != 2 {
		t.Fatalf("fix sessions = %d, want the suggestion round and the push repair only", got)
	}
	if res.LastFindings.MergeEligible() {
		t.Fatalf("findings = %+v; a head with a blocker must stay unmergeable", res.LastFindings)
	}
}
