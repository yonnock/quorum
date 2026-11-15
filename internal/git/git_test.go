package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/proc"
)

// conflictRepo builds a real repository whose main and feature branches edited
// the same line, plus a clean branch that did not.
func conflictRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.name", "Test User")
	run("config", "user.email", "test@example.invalid")
	write("shared.txt", "base\n")
	run("add", "shared.txt")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	write("shared.txt", "feature\n")
	run("commit", "-q", "-am", "feature change")
	run("checkout", "-q", "-b", "clean", "main")
	write("other.txt", "clean\n")
	run("add", "other.txt")
	run("commit", "-q", "-m", "clean change")
	run("checkout", "-q", "main")
	write("shared.txt", "main\n")
	run("commit", "-q", "-am", "main change")
	return dir
}

func TestUnmergedFilesReportsMergeConflicts(t *testing.T) {
	dir := conflictRepo(t)
	g := New("git")
	cmd := exec.Command("git", "checkout", "-q", "feature")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout feature: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "merge", "main")
	cmd.Dir = dir
	mergeOutput, mergeErr := cmd.CombinedOutput()
	if mergeErr == nil {
		t.Fatal("fixture merge unexpectedly succeeded")
	}
	got, err := g.UnmergedFiles(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "shared.txt" {
		t.Fatalf("unmerged files = %q, want shared.txt; merge output:\n%s", got, mergeOutput)
	}
}

// The probe must see a conflict in a repository where the two sides also
// changed different files of one directory, which is every real repository.
// `git merge-tree --quiet` exits 0 for that shape (seen with git 2.54) even
// though the merge conflicts, and a conflict-only base update then leaves a
// conflicting branch alone.
func TestMergeConflictsSeesAConflictBesideACleanDirectoryMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	write("app/shared.txt", "base\n")
	write("docs/one.txt", "base\n")
	write("docs/two.txt", "base\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	write("app/shared.txt", "feature\n")
	write("docs/one.txt", "feature\n")
	run("commit", "-q", "-am", "feature change")
	run("checkout", "-q", "-b", "clean", "main")
	write("docs/one.txt", "clean\n")
	run("commit", "-q", "-am", "clean change")
	run("checkout", "-q", "main")
	write("app/shared.txt", "main\n")
	write("docs/two.txt", "main\n")
	run("commit", "-q", "-am", "main change")

	g := New("git")
	conflicts, err := g.MergeConflicts(context.Background(), dir, "feature", "main")
	if err != nil || !conflicts {
		t.Fatalf("MergeConflicts(feature, main) = %v, %v; want a conflict", conflicts, err)
	}
	conflicts, err = g.MergeConflicts(context.Background(), dir, "clean", "main")
	if err != nil || conflicts {
		t.Fatalf("MergeConflicts(clean, main) = %v, %v; want a clean merge", conflicts, err)
	}
	if _, err := g.MergeConflicts(context.Background(), dir, "feature", "no-such-branch"); err == nil {
		t.Fatal("MergeConflicts with an unknown revision reported an answer")
	}
}

func TestCleanUntrackedPreservesTrackedAndIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q")
	write(".gitignore", "ignored/\n")
	write("tracked.txt", "tracked\n")
	run("add", ".gitignore", "tracked.txt")
	run("commit", "-q", "-m", "fixture")
	write("generated/cache.db", "generated\n")
	write("ignored/cache.db", "ignored\n")

	g := New("git")
	if err := g.CleanUntracked(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("untracked directory remains: %v", err)
	}
	for _, path := range []string{"tracked.txt", "ignored/cache.db"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}

func TestShowFileDistinguishesMissingPathsFromInvalidRevisions(t *testing.T) {
	dir := conflictRepo(t)
	g := New("git")

	content, ok, err := g.ShowFile(context.Background(), dir, "main", "missing.txt")
	if err != nil || ok || content != "" {
		t.Fatalf("ShowFile missing path = %q, %v, %v; want empty, false, nil", content, ok, err)
	}

	if _, _, err := g.ShowFile(context.Background(), dir, "not-a-revision", "shared.txt"); err == nil {
		t.Fatal("ShowFile accepted an invalid revision as a missing file")
	}
}

func TestShowFilePropagatesPathProbeFailures(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
case "$*" in
  "rev-parse -q --verify main^{tree}") echo tree ;;
  "ls-tree --name-only main -- blocked.txt") echo unavailable >&2; exit 1 ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := New(bin).ShowFile(context.Background(), t.TempDir(), "main", "blocked.txt"); err == nil {
		t.Fatal("ShowFile accepted a path-probe failure as a missing file")
	}
}

// The diverged-checkout stop leans on this distinction: a local branch that is
// merely behind origin may proceed, one with commits of its own may not.
func TestIsAncestorDistinguishesBehindFromDiverged(t *testing.T) {
	dir := conflictRepo(t)
	g := G{Bin: "git"}
	ctx := context.Background()

	if ok, err := g.IsAncestor(ctx, dir, "main~1", "main"); err != nil || !ok {
		t.Fatalf("IsAncestor(main~1, main) = (%v, %v), want true", ok, err)
	}
	if ok, err := g.IsAncestor(ctx, dir, "feature", "main"); err != nil || ok {
		t.Fatalf("IsAncestor(feature, main) = (%v, %v), want false for a diverged branch", ok, err)
	}
	if _, err := g.IsAncestor(ctx, dir, "not-a-rev", "main"); err == nil {
		t.Fatal("an unresolvable revision was not reported as an error")
	}
}

// The round summary and the fix-log comment list a round's commits with this.
// A round that merged the base branch must not claim the base's commits as its
// own work: only the first-parent line belongs to the round.
func TestLogOnelineFollowsFirstParent(t *testing.T) {
	dir := conflictRepo(t)
	g := G{Bin: "git"}
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// The clean branch merges main, which brings in "main change".
	run("checkout", "-q", "clean")
	run("merge", "-q", "--no-edit", "main")

	out := g.LogOneline(ctx, dir, "main~1..clean")
	if !strings.Contains(out, "clean change") {
		t.Fatalf("the branch's own commit is missing:\n%s", out)
	}
	if strings.Contains(out, "main change") {
		t.Fatalf("a commit merged in from the base is listed as the branch's own:\n%s", out)
	}
}

func TestIsolateHooksSnapshotsAWorktreeAwayFromSharedHookChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	shared := filepath.Join(dir, ".git", "hooks", "pre-push")
	if err := os.WriteFile(shared, []byte("#!/bin/sh\necho first\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	g := New("git")
	isolated, err := g.IsolateHooks(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\necho first\n"
	isolatedHook := filepath.Join(isolated, "pre-push")
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != want {
		t.Fatalf("isolated hook = %q, %v; want %q", data, err, want)
	}
	if info, err := os.Stat(isolatedHook); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("isolated hook mode = %v, %v; want 0755", info, err)
	}

	if err := os.WriteFile(shared, []byte("#!/bin/sh\necho second\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != want {
		t.Fatalf("shared rewrite reached isolated hook: %q, %v", data, err)
	}
	resumed, err := g.IsolateHooks(context.Background(), worktree)
	if err != nil || resumed != isolated {
		t.Fatalf("resumed hook snapshot = %q, %v; want %q", resumed, err, isolated)
	}
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != want {
		t.Fatalf("resume replaced the isolated hook: %q, %v", data, err)
	}
	resolved, err := g.WithHooksPath(isolated).PrePushPath(context.Background(), worktree)
	if err != nil || resolved != isolatedHook {
		t.Fatalf("scoped pre-push path = %q, %v; want %q", resolved, err, isolatedHook)
	}
}

func TestIsolateConfigCopiesTheSharedConfigOnce(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	g := New("git")
	isolated, err := g.IsolateConfig(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(isolated)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "config", "core.hooksPath", "/dev/null")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, out)
	}
	after, err := os.ReadFile(isolated)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("shared config change reached the isolated copy")
	}
	resumed, err := g.IsolateConfig(context.Background(), worktree)
	if err != nil || resumed != isolated {
		t.Fatalf("resumed config = %q, %v; want %q", resumed, err, isolated)
	}
}

func TestIsolateHooksCopiesConfiguredRelativePathsAndInternalSymlinks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "config", "core.hooksPath", ".githooks")
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, out)
	}
	hooks := filepath.Join(worktree, ".githooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "runner"), []byte("#!/bin/sh\necho stable\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("runner", filepath.Join(hooks, "pre-push")); err != nil {
		t.Fatal(err)
	}

	isolated, err := New("git").IsolateHooks(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	isolatedHook := filepath.Join(isolated, "pre-push")
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != "#!/bin/sh\necho stable\n" {
		t.Fatalf("isolated symlink = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "runner"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != "#!/bin/sh\necho stable\n" {
		t.Fatalf("configured hook rewrite reached isolated symlink: %q, %v", data, err)
	}
}

func TestIsolateHooksAcceptsAMissingConfiguredHookDirectory(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "config", "core.hooksPath", ".missing-hooks")
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, out)
	}

	isolated, err := New("git").IsolateHooks(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(isolated)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("isolated missing hook directory contains %d entries", len(entries))
	}
}

func TestPushPinsTheVerifiedSHAAndSkipsTheSecondHookRun(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsPath + `"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if out, err := New(bin).Push(context.Background(), dir, "origin", "feature/crumb-tray", "verified-sha"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "push\n-q\n--no-verify\norigin\nverified-sha:refs/heads/feature/crumb-tray\n"
	if got := string(data); got != want {
		t.Fatalf("git arguments = %q, want %q", got, want)
	}
}

func TestLsRemoteReportsAMissingRefAsErrRefNotFound(t *testing.T) {
	dir := conflictRepo(t)
	g := New("git")
	ctx := context.Background()
	if sha, err := g.LsRemote(ctx, dir, dir, "refs/heads/main"); err != nil || sha == "" {
		t.Fatalf("existing ref: sha = %q, err = %v", sha, err)
	}
	_, err := g.LsRemote(ctx, dir, dir, "refs/heads/missing")
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("err = %v, want ErrRefNotFound", err)
	}
	if want := "could not resolve refs/heads/missing on " + dir; err.Error() != want {
		t.Errorf("message = %q, want %q", err, want)
	}
}

// git runs in its own process group, so cancelling a run takes what git
// started with it: a pre-push hook in the middle of a test suite, a transport.
// Killing git alone left those running.
func TestCancelReachesWhatGitStarted(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(bin).Push(ctx, dir, "origin", "main", "abc123")
		done <- err
	}()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if pid == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the hook never started")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Push returned %v, want the cancellation", err)
	}
	if proc.Alive(pid) {
		t.Fatalf("the hook's child %d survived the cancelled push", pid)
	}
}

// flakyGit is a git whose remote fails with stderr the first failures times
// and then answers; count holds how often it was asked.
func flakyGit(t *testing.T, failures int, stderr string) (g G, count string) {
	t.Helper()
	dir := t.TempDir()
	count = filepath.Join(dir, "count")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
n=$(cat '` + count + `' 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > '` + count + `'
if [ "$n" -le ` + strconv.Itoa(failures) + ` ]; then
  echo "` + stderr + `" >&2
  exit 128
fi
printf '0123456789abcdef0123456789abcdef01234567\trefs/heads/main\n'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g = New(bin)
	g.backoff = time.Millisecond
	return g, count
}

// A name lookup that fails for a moment is the network's problem, not the
// run's: one failed ls-remote used to end a run that was half an hour in.
func TestRemoteReadsOutlastANetworkBlip(t *testing.T) {
	const blip = "fatal: unable to access 'https://example.invalid/acme/api.git/': Could not resolve host: example.invalid"
	for name, read := range map[string]func(G) error{
		"ls-remote": func(g G) error {
			sha, err := g.LsRemote(context.Background(), t.TempDir(), "origin", "refs/heads/main")
			if err == nil && sha != "0123456789abcdef0123456789abcdef01234567" {
				t.Errorf("ls-remote = %q", sha)
			}
			return err
		},
		"fetch":     func(g G) error { return g.FetchBranch(context.Background(), t.TempDir(), "main") },
		"fetch all": func(g G) error { return g.FetchAll(context.Background(), t.TempDir()) },
	} {
		g, count := flakyGit(t, 2, blip)
		if err := read(g); err != nil {
			t.Errorf("%s after two blips = %v, want the third attempt's answer", name, err)
		}
		if calls, _ := os.ReadFile(count); strings.TrimSpace(string(calls)) != "3" {
			t.Errorf("%s asked %q times, want 3", name, strings.TrimSpace(string(calls)))
		}
	}
}

// Only the network's failures are worth repeating, and not forever.
func TestRemoteReadsGiveUp(t *testing.T) {
	g, count := flakyGit(t, 100, "fatal: repository 'https://example.invalid/acme/api.git/' not found")
	if _, err := g.LsRemote(context.Background(), t.TempDir(), "origin", "refs/heads/main"); err == nil {
		t.Fatal("a missing repository read as an answer")
	}
	if calls, _ := os.ReadFile(count); strings.TrimSpace(string(calls)) != "1" {
		t.Errorf("a missing repository was asked for %q times, want 1", strings.TrimSpace(string(calls)))
	}

	g, count = flakyGit(t, 100, "fatal: unable to access 'https://example.invalid/acme/api.git/': Could not resolve host: example.invalid")
	_, err := g.LsRemote(context.Background(), t.TempDir(), "origin", "refs/heads/main")
	if err == nil || !strings.Contains(err.Error(), "Could not resolve host") {
		t.Fatalf("a remote that stays unreachable = %v, want its last failure", err)
	}
	if calls, _ := os.ReadFile(count); strings.TrimSpace(string(calls)) != strconv.Itoa(remoteReadAttempts) {
		t.Errorf("an unreachable remote was asked %q times, want %d", strings.TrimSpace(string(calls)), remoteReadAttempts)
	}
}
