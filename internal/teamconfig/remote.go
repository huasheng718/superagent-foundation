package teamconfig

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

func ValidateURL(raw, fixtureRoot string, allowFile bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("registered TeamAI source requires a credential-free HTTPS URL")
	}
	if u.Scheme == "https" && u.Hostname() != "" && u.Path != "" {
		return nil
	}
	if u.Scheme != "file" || !allowFile || u.Host != "" || fixtureRoot == "" || !filepath.IsAbs(fixtureRoot) || !filepath.IsAbs(u.Path) {
		return fmt.Errorf("registered TeamAI source requires HTTPS; local fixtures need deployment opt-in")
	}
	root, err := filepath.EvalSymlinks(fixtureRoot)
	if err != nil {
		return fmt.Errorf("TeamAI fixture root is unavailable")
	}
	p, err := filepath.EvalSymlinks(u.Path)
	if err != nil {
		return fmt.Errorf("TeamAI fixture repository is unavailable")
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("registered fixture repository must be inside the deployment fixture root")
	}
	return nil
}
