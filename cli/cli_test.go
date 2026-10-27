// Tests for the command-line front end.
//
// The CLI is a thin adapter, so these tests focus on parsing, output
// formatting, error reporting, and session control — not on service
// behaviour, which the yodelr package's tests cover.
package cli

import (
	"strings"
	"testing"

	"github.com/kevin907/yodelr-updated/yodelr"
)

func runCLI(script string) string {
	var out strings.Builder
	New(&out).Run(strings.NewReader(script), false)
	return out.String()
}

func assertContains(t *testing.T, output, want string) {
	t.Helper()
	if !strings.Contains(output, want) {
		t.Fatalf("output does not contain %q:\n%s", want, output)
	}
}

// assertOrdered asserts first appears before second in output.
func assertOrdered(t *testing.T, output, first, second string) {
	t.Helper()
	i, j := strings.Index(output, first), strings.Index(output, second)
	if i < 0 || j < 0 || i >= j {
		t.Fatalf("expected %q before %q in:\n%s", first, second, output)
	}
}

func TestRoundtripAddAndQuery(t *testing.T) {
	output := runCLI(
		"add_user kevin\n" +
			"add_post kevin 10 hello #world it's nice\n" +
			"add_post kevin 20 more #world news\n" +
			"posts_for_user kevin\n" +
			"posts_for_topic world\n")
	assertContains(t, output, "ok: user 'kevin' registered")
	assertContains(t, output, "ok: post stored at timestamp 10")
	// Newest first, apostrophes and '#' taken literally.
	assertOrdered(t, output,
		"1. more #world news", "2. hello #world it's nice")
}

func TestAutoTimestampAndTrending(t *testing.T) {
	output := runCLI(
		"add_user ada\n" +
			"add_post ada auto #a #b\n" +
			"add_post ada auto #a\n" +
			"trending 1 2\n")
	assertContains(t, output, "ok: post stored at timestamp 1")
	assertContains(t, output, "ok: post stored at timestamp 2")
	assertOrdered(t, output, "1. a", "2. b")
}

func TestErrorsAreReportedAndSessionContinues(t *testing.T) {
	output := runCLI(
		"add_user ada\n" +
			"add_user ada\n" + // duplicate -> error
			"posts_for_user ghost\n" + // unknown -> error
			"add_post ada\n" + // no text -> usage error
			"add_post ada 5\n" + // timestamp but no text -> usage error
			"frobnicate\n" + // unknown command
			"add_post ada 1 still #alive\n" +
			"posts_for_topic alive\n")
	assertContains(t, output, "error: user already exists: 'ada'")
	assertContains(t, output, "error: unknown user: 'ghost'")
	if n := strings.Count(output,
		"usage: add_post <name> [timestamp|auto]"); n != 2 {
		t.Fatalf("expected 2 usage errors, got %d in:\n%s", n, output)
	}
	assertContains(t, output, "unknown command 'frobnicate'")
	assertContains(t, output, "1. still #alive") // the loop survived it all
}

func TestTimestampIsOptionalAndDefaultsToAuto(t *testing.T) {
	output := runCLI(
		"add_user ada\n" +
			"add_post ada no timestamp given here\n" + // -> auto = 1
			"add_post ada 10 explicit timestamp\n" + // -> 10
			"add_post ada back to #implicit\n" + // -> auto = 11
			"add_post ada 42 is the answer\n" + // 42 is read as the ts
			"add_post ada auto 42 is the answer\n" + // auto, text keeps '42'
			"posts_for_user ada\n")
	for _, stamp := range []string{"1", "10", "11", "42", "43"} {
		assertContains(t, output, "ok: post stored at timestamp "+stamp)
	}
	assertContains(t, output, "1. 42 is the answer") // newest, ts 43
	assertContains(t, output, "2. is the answer")    // ts 42 ate the '42'
}

func TestEmptyResultsAndDelete(t *testing.T) {
	output := runCLI(
		"add_user ada\n" +
			"add_post ada 1 some #thing\n" +
			"delete_user ada\n" +
			"posts_for_topic thing\n" +
			"trending 1 1\n")
	assertContains(t, output, "ok: user 'ada' and all their posts deleted")
	if n := strings.Count(output, "(none)"); n != 2 {
		t.Fatalf("expected 2 empty results, got %d in:\n%s", n, output)
	}
}

func TestQuitStopsProcessing(t *testing.T) {
	output := runCLI(
		"add_user ada\n" +
			"quit\n" +
			"add_user bob\n")
	assertContains(t, output, "ok: user 'ada' registered")
	if strings.Contains(output, "bob") {
		t.Fatalf("processing continued after quit:\n%s", output)
	}
}

func TestStatsCommand(t *testing.T) {
	output := runCLI(
		"stats\n" +
			"add_user ada\n" +
			"add_post ada 1 hello #world\n" +
			"stats\n" +
			"delete_user ada\n" +
			"stats\n")
	if n := strings.Count(output, "posts : 0 (live)"); n != 2 { // first & last
		t.Fatalf("expected 2 zero-post stats, got %d in:\n%s", n, output)
	}
	assertContains(t, output, "users : 1")
	assertContains(t, output, "posts : 1 (live)")
	assertContains(t, output, "topics: 1 (with at least one live post)")
}

func TestDemoReplaysReadmeExample(t *testing.T) {
	output := runCLI("demo\n")
	assertContains(t, output, "> trending 1 3")
	// README: getTrendingTopics(1, 3) -> ["steak", "chilling"]
	assertOrdered(t, output, "1. steak", "2. chilling")
	assertContains(t, output, "1. ugh! this #steak tasted like dog food")
}

func TestCommentsAndBlankLinesAreIgnored(t *testing.T) {
	output := runCLI("# a comment\n\nadd_user ada\n")
	assertContains(t, output, "ok: user 'ada' registered")
	if strings.Contains(output, "error") {
		t.Fatalf("unexpected error in:\n%s", output)
	}
}

func TestCLICanWrapAPreloadedService(t *testing.T) {
	// Bulk loaders build a service in-process and hand it to the CLI;
	// the wrapped data must be queryable and 'auto' must continue
	// above the already-stored timestamps.
	service := yodelr.NewService()
	if err := service.AddUser("ada"); err != nil {
		t.Fatal(err)
	}
	if err := service.AddPost("ada", "pre #loaded data", 7); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	c := NewWithService(&out, service)
	c.LastTimestamp = 7
	c.Run(strings.NewReader(
		"posts_for_topic loaded\n"+
			"add_post ada and now live\n"+
			"posts_for_user ada\n"), false)
	output := out.String()
	assertContains(t, output, "1. pre #loaded data")
	assertContains(t, output, "ok: post stored at timestamp 8")
	assertOrdered(t, output, "1. and now live", "2. pre #loaded data")
}
