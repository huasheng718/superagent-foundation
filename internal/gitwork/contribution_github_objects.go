package gitwork

import (
	"bytes"
	"context"
	"crypto/sha1" // Git SHA1 object identity, not an authentication primitive.
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type GitHubTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type GitHubCommitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type GitHubCommitObject struct {
	Message   string               `json:"message"`
	Tree      string               `json:"tree"`
	Parents   []string             `json:"parents"`
	Author    GitHubCommitIdentity `json:"author"`
	Committer GitHubCommitIdentity `json:"committer"`
}

// GitHubContributionObject is a lossless proposal to the GitHub Git Data API.
// The provider must return this exact SHA before any ref may be created.
type GitHubContributionObject struct {
	SHA     string
	Type    string
	Content []byte
	Entries []GitHubTreeEntry
	Commit  *GitHubCommitObject
}

var contributionIdentityPattern = regexp.MustCompile(`^(.*) <([^<>]+)> ([0-9]+) ([+-][0-9]{4})$`)

func githubCommitIdentity(raw string) (GitHubCommitIdentity, error) {
	var value GitHubCommitIdentity
	parts := contributionIdentityPattern.FindStringSubmatch(raw)
	if len(parts) != 5 || parts[1] == "" || !utf8.ValidString(raw) {
		return value, errors.New("unsupported Git commit identity")
	}
	stamp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return value, errors.New("unsupported Git commit timestamp")
	}
	hours, _ := strconv.Atoi(parts[4][1:3])
	minutes, _ := strconv.Atoi(parts[4][3:5])
	if hours > 23 || minutes > 59 {
		return value, errors.New("invalid Git commit timezone")
	}
	offset := (hours*60 + minutes) * 60
	if parts[4][0] == '-' {
		offset = -offset
	}
	value = GitHubCommitIdentity{Name: parts[1], Email: parts[2], Date: time.Unix(stamp, 0).In(time.FixedZone("", offset)).Format(time.RFC3339)}
	return value, nil
}

func githubCommitObject(raw []byte) (*GitHubCommitObject, error) {
	head, message, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok || !utf8.Valid(raw) {
		return nil, errors.New("unsupported Git commit encoding")
	}
	value := &GitHubCommitObject{Message: string(message), Parents: []string{}}
	counts := map[string]int{}
	for _, line := range strings.Split(string(head), "\n") {
		key, text, ok := strings.Cut(line, " ")
		if !ok {
			return nil, errors.New("invalid Git commit header")
		}
		counts[key]++
		switch key {
		case "tree":
			if counts[key] != 1 || !fullCommitSHA(text) {
				return nil, errors.New("invalid Git commit tree")
			}
			value.Tree = text
		case "parent":
			if !fullCommitSHA(text) {
				return nil, errors.New("invalid Git commit parent")
			}
			value.Parents = append(value.Parents, text)
		case "author", "committer":
			if counts[key] != 1 {
				return nil, errors.New("duplicate Git commit identity")
			}
			identity, err := githubCommitIdentity(text)
			if err != nil {
				return nil, err
			}
			if key == "author" {
				value.Author = identity
			} else {
				value.Committer = identity
			}
		default:
			return nil, errors.New("Git Data publication cannot reproduce this commit header")
		}
	}
	if counts["tree"] != 1 || counts["author"] != 1 || counts["committer"] != 1 {
		return nil, errors.New("Git commit identity or tree missing")
	}
	// The Git Data API rebuilds canonical commit headers from structured
	// fields. Reordered identities, noncanonical timestamps and -0000 offsets
	// are valid in some local Git objects but cannot survive that rebuilding.
	// Reject them before creating any remote immutable objects.
	var canonical strings.Builder
	fmt.Fprintf(&canonical, "tree %s\n", value.Tree)
	for _, parent := range value.Parents {
		fmt.Fprintf(&canonical, "parent %s\n", parent)
	}
	for _, identity := range []struct {
		kind  string
		value GitHubCommitIdentity
	}{{"author", value.Author}, {"committer", value.Committer}} {
		date, err := time.Parse(time.RFC3339, identity.value.Date)
		if err != nil {
			return nil, errors.New("Git Data publication cannot reproduce this commit date")
		}
		fmt.Fprintf(&canonical, "%s %s <%s> %d %s\n", identity.kind, identity.value.Name, identity.value.Email, date.Unix(), date.Format("-0700"))
	}
	canonical.WriteByte('\n')
	canonical.WriteString(value.Message)
	if canonical.String() != string(raw) {
		return nil, errors.New("Git Data publication cannot reproduce these commit header bytes")
	}
	return value, nil
}

// GitHubContributionObjects serializes a fresh verified checkout in dependency
// order. It preserves commit messages, identities, timestamps, trees and parent
// order; signatures/unknown headers fail explicitly rather than changing SHA.
// No network, credential, hook, user config or remote is used here.
func GitHubContributionObjects(ctx context.Context, checkout, baseSHA, resultSHA string) ([]GitHubContributionObject, error) {
	if !fullCommitSHA(baseSHA) || !fullCommitSHA(resultSHA) {
		return nil, errors.New("invalid Git Data contribution SHAs")
	}
	if err := validateContributionRepository(checkout); err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "agentflow-git-data-home-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	run := func(args ...string) ([]byte, error) {
		out, err := contributionGit(ctx, checkout, home, "", args...).Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("Git Data object read failed")
		}
		return out, nil
	}
	head, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(head)) != resultSHA {
		return nil, errors.New("Git Data checkout SHA mismatch")
	}
	dirty, err := run("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if len(dirty) > 0 {
		return nil, errors.New("Git Data checkout must be clean")
	}
	if _, err := run("merge-base", "--is-ancestor", baseSHA, resultSHA); err != nil {
		return nil, errors.New("Git Data result does not descend from base")
	}
	list, err := run("rev-list", "--reverse", "--topo-order", baseSHA+".."+resultSHA)
	if err != nil {
		return nil, err
	}
	objects := []GitHubContributionObject{}
	seen := map[string]bool{}
	var total int64
	read := func(kind, sha string) ([]byte, error) {
		size, err := run("cat-file", "-s", sha)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
		if err != nil || n < 0 || n > 64<<20 || total+n > 64<<20 || len(seen) >= 100000 {
			return nil, errors.New("Git Data object budget exceeded")
		}
		raw, err := run("cat-file", kind, sha)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != n {
			return nil, errors.New("Git Data object size changed")
		}
		digest := sha1.Sum(append([]byte(fmt.Sprintf("%s %d\x00", kind, n)), raw...))
		if hex.EncodeToString(digest[:]) != sha {
			return nil, errors.New("Git Data raw object hash mismatch")
		}
		total += n
		return raw, nil
	}
	var visitTree func(string) error
	visitTree = func(sha string) error {
		if seen[sha] {
			return nil
		}
		seen[sha] = true
		raw, err := read("tree", sha)
		if err != nil {
			return err
		}
		listing, err := run("ls-tree", "-z", sha)
		if err != nil {
			return err
		}
		entries := []GitHubTreeEntry{}
		for _, line := range bytes.Split(listing, []byte{0}) {
			if len(line) == 0 {
				continue
			}
			meta, path, ok := bytes.Cut(line, []byte{'\t'})
			parts := strings.Fields(string(meta))
			if !ok || len(parts) != 3 || !fullCommitSHA(parts[2]) || !utf8.Valid(path) {
				return errors.New("unsupported Git tree entry")
			}
			entry := GitHubTreeEntry{Path: string(path), Mode: parts[0], Type: parts[1], SHA: parts[2]}
			if entry.Type == "tree" && entry.Mode == "040000" {
				if err := visitTree(entry.SHA); err != nil {
					return err
				}
			} else if entry.Type == "blob" && (entry.Mode == "100644" || entry.Mode == "100755" || entry.Mode == "120000") {
				if !seen[entry.SHA] {
					seen[entry.SHA] = true
					content, err := read("blob", entry.SHA)
					if err != nil {
						return err
					}
					objects = append(objects, GitHubContributionObject{SHA: entry.SHA, Type: "blob", Content: content})
				}
			} else {
				return errors.New("Git Data publication does not support this tree mode")
			}
			entries = append(entries, entry)
		}
		objects = append(objects, GitHubContributionObject{SHA: sha, Type: "tree", Content: raw, Entries: entries})
		return nil
	}
	for _, sha := range strings.Fields(string(list)) {
		if seen[sha] {
			continue
		}
		seen[sha] = true
		raw, err := read("commit", sha)
		if err != nil {
			return nil, err
		}
		commit, err := githubCommitObject(raw)
		if err != nil {
			return nil, err
		}
		if err := visitTree(commit.Tree); err != nil {
			return nil, err
		}
		objects = append(objects, GitHubContributionObject{SHA: sha, Type: "commit", Content: raw, Commit: commit})
	}
	return objects, nil
}
