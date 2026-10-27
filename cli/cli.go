// Package cli is the interactive command-line front end for the Yodelr
// service.
//
// It is a thin human-facing adapter over yodelr.Service — it contains
// no service logic of its own. It is a CLI rather than an HTTP server
// on purpose: the challenge rules forbid network use (directly or
// indirectly) and assume a single thread calls the API, both of which a
// REPL over stdin/stdout satisfies naturally.
//
// Notes:
//   - post text runs to the end of the line — no quoting needed, '#'
//     and apostrophes are taken literally;
//   - the timestamp is optional and defaults to "auto" (previous + 1),
//     since the service requires strictly increasing timestamps; a
//     second token that is an integer or "auto" is read as the
//     timestamp, so a post whose text begins with a bare number needs
//     "auto" (or an explicit timestamp) in front of it;
//   - user names and topics cannot contain whitespace in the CLI (the
//     service itself does not care).
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kevin907/yodelr-updated/yodelr"
)

const help = `commands:
  add_user <name>                      register a user
  add_post <name> [timestamp|auto] <text ...>
                                       store a post; text runs to end of line,
                                       timestamp defaults to auto (previous+1).
                                       Text starting with a number? put 'auto'
                                       (or a timestamp) before it.
  delete_user <name>                   delete a user and all their posts
  posts_for_user <name>                the user's posts, newest first
  posts_for_topic <topic>              posts mentioning #topic, newest first
  trending <from> <to>                 topics in [from, to], most mentioned first
  stats                                totals: users, live posts, live topics
  demo                                 replay the example from the README
  help                                 show this help
  quit | exit                          leave
`

var demo = []string{
	"add_user john",
	"add_post john 1 just #chilling today",
	"add_post john 2 eating #steak for dinner",
	"add_post john 3 ugh! this #steak tasted like dog food",
	"posts_for_user john",
	"posts_for_topic steak",
	"trending 1 3",
	"trending 2 3",
}

// splitMax splits s on runs of whitespace into at most n fields, the
// last field keeping the remaining text verbatim after the separator
// run (Python's str.split(maxsplit=n-1) on a stripped string).
func splitMax(s string, n int) []string {
	var fields []string
	for s = strings.TrimLeft(s, " \t"); s != ""; s = strings.TrimLeft(s, " \t") {
		if len(fields) == n-1 {
			fields = append(fields, s)
			break
		}
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
	return fields
}

// comma formats n with thousands separators ("1234567" -> "1,234,567").
func comma(n int) string {
	s := strconv.Itoa(n)
	start := 0
	if s[0] == '-' {
		start = 1
	}
	var b strings.Builder
	b.WriteString(s[:start])
	for i := start; i < len(s); i++ {
		if i > start && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// CLI runs Yodelr commands read line by line against one service.
type CLI struct {
	// Service is the wrapped service. An existing (e.g. pre-loaded)
	// service may be set; callers doing so should also set
	// LastTimestamp so 'auto' stays ahead of the posts already stored.
	Service *yodelr.Service
	// LastTimestamp is the most recent successfully stored timestamp;
	// 'auto' resolves to LastTimestamp + 1.
	LastTimestamp int64

	out io.Writer
}

// New returns a CLI over a fresh service, writing to out.
func New(out io.Writer) *CLI {
	return &CLI{Service: yodelr.NewService(), out: out}
}

// NewWithService returns a CLI wrapping an existing service.
func NewWithService(out io.Writer, service *yodelr.Service) *CLI {
	return &CLI{Service: service, out: out}
}

func (c *CLI) print(text string) {
	fmt.Fprintln(c.out, text)
}

func (c *CLI) printItems(items []string) {
	if len(items) == 0 {
		c.print("  (none)")
	}
	for i, item := range items {
		c.print(fmt.Sprintf("  %d. %s", i+1, item))
	}
}

func (c *CLI) parseTimestamp(raw string) (int64, error) {
	if raw == "auto" {
		return c.LastTimestamp + 1, nil
	}
	ts, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("timestamp must be an integer or 'auto', "+
			"got '%s'", raw)
	}
	return ts, nil
}

// isTimestampToken reports whether token names a timestamp: "auto", an
// integer, or an integer too large for int64 (reported as an error by
// parseTimestamp's caller rather than silently treated as post text).
func isTimestampToken(token string) bool {
	if token == "auto" {
		return true
	}
	_, err := strconv.ParseInt(token, 10, 64)
	return err == nil || errors.Is(err, strconv.ErrRange)
}

// Execute runs one command line; it returns false when the session
// should end.
func (c *CLI) Execute(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return true
	}
	parts := splitMax(line, 2)
	command := strings.ToLower(parts[0])
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}
	cont, err := c.dispatch(command, rest)
	if err != nil {
		c.print("error: " + err.Error())
	}
	return cont
}

func (c *CLI) dispatch(command, rest string) (bool, error) {
	switch command {
	case "add_user":
		if rest == "" || len(strings.Fields(rest)) != 1 {
			return true, errors.New("usage: add_user <name>")
		}
		if err := c.Service.AddUser(rest); err != nil {
			return true, err
		}
		c.print(fmt.Sprintf("ok: user '%s' registered", rest))
	case "add_post":
		const usage = "usage: add_post <name> [timestamp|auto] <text ...>"
		fields := splitMax(rest, 3)
		if len(fields) < 2 {
			return true, errors.New(usage)
		}
		user := fields[0]
		// A second token that is an integer or 'auto' is the
		// timestamp; otherwise the timestamp is implied ('auto') and
		// the text starts at the second token.
		var timestamp int64
		var text string
		if isTimestampToken(fields[1]) {
			if len(fields) < 3 {
				return true, errors.New(usage + " (post text is missing)")
			}
			ts, err := c.parseTimestamp(fields[1])
			if err != nil {
				return true, err
			}
			timestamp = ts
			text = fields[2]
		} else {
			timestamp = c.LastTimestamp + 1
			text = splitMax(rest, 2)[1]
		}
		if err := c.Service.AddPost(user, text, timestamp); err != nil {
			return true, err
		}
		c.LastTimestamp = timestamp
		c.print(fmt.Sprintf("ok: post stored at timestamp %d", timestamp))
	case "delete_user":
		if rest == "" || len(strings.Fields(rest)) != 1 {
			return true, errors.New("usage: delete_user <name>")
		}
		if err := c.Service.DeleteUser(rest); err != nil {
			return true, err
		}
		c.print(fmt.Sprintf(
			"ok: user '%s' and all their posts deleted", rest))
	case "posts_for_user":
		posts, err := c.Service.GetPostsForUser(rest)
		if err != nil {
			return true, err
		}
		c.printItems(posts)
	case "posts_for_topic":
		c.printItems(c.Service.GetPostsForTopic(rest))
	case "trending":
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			return true, errors.New("usage: trending <from> <to>")
		}
		from, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return true, fmt.Errorf("'%s' is not an integer", fields[0])
		}
		to, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return true, fmt.Errorf("'%s' is not an integer", fields[1])
		}
		c.printItems(c.Service.GetTrendingTopics(from, to))
	case "stats":
		c.print(fmt.Sprintf("  users : %s", comma(c.Service.UserCount())))
		c.print(fmt.Sprintf("  posts : %s (live)",
			comma(c.Service.PostCount())))
		c.print(fmt.Sprintf("  topics: %s (with at least one live post)",
			comma(c.Service.TopicCount())))
	case "demo":
		c.print("replaying the README example " +
			"(a second run shows duplicate-user rejection):")
		for _, demoLine := range demo {
			c.print("> " + demoLine)
			c.Execute(demoLine)
		}
	case "help":
		fmt.Fprint(c.out, help)
	case "quit", "exit":
		return false, nil
	default:
		c.print(fmt.Sprintf("unknown command '%s' — type 'help'", command))
	}
	return true, nil
}

// Run executes commands from source until it is exhausted or a command
// ends the session. When interactive, it prints a banner and prompts.
func (c *CLI) Run(source io.Reader, interactive bool) {
	if interactive {
		c.print("Yodelr CLI — type 'help' for commands, " +
			"'demo' for the README example")
	}
	scanner := bufio.NewScanner(source)
	for {
		if interactive {
			fmt.Fprint(c.out, "yodelr> ")
		}
		if !scanner.Scan() || !c.Execute(scanner.Text()) {
			break
		}
	}
	if interactive {
		c.print("bye")
	}
}
