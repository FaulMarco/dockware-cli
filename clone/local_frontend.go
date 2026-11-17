package clone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// FrontendImage is the node image of the frontend service: the configured
// one, else the pinned node major (--node, node:), else the major the
// frontend (or the project) pins, else the shop's node, else the current
// LTS. "" without a frontend.
func (l *Local) FrontendImage() string {
	f := l.Frontend
	if f == nil {
		return ""
	}
	if f.Image != "" {
		return f.Image
	}
	if l.NodePinned && l.NodeVersion != "" {
		return "node:" + l.NodeVersion
	}
	if v := DetectNodeVersion(filepath.Join(l.CodeDir, f.Path), l.CodeDir); v != "" {
		return "node:" + v
	}
	if l.NodeVersion != "" {
		return "node:" + l.NodeVersion
	}
	return "node:lts"
}

// AccessKeys lists sales channel access keys from the local (cloned)
// database by name: the translation in the channel's language, else the one
// in the system language, else any; every channel is also found by its hex id.
func (l *Local) AccessKeys(ctx context.Context) (map[string]string, []string, error) {
	out, err := l.SQL(ctx, "SELECT COALESCE(t.name, (SELECT t2.name FROM sales_channel_translation t2 WHERE t2.sales_channel_id = sc.id AND t2.name IS NOT NULL ORDER BY t2.language_id = UNHEX('2FBB5FE2E29A4D70AA5854CE7CE3E20B') DESC, t2.language_id LIMIT 1), LOWER(HEX(sc.id))), sc.access_key, LOWER(HEX(sc.type_id)), LOWER(HEX(sc.id)) FROM sales_channel sc LEFT JOIN sales_channel_translation t ON t.sales_channel_id = sc.id AND t.language_id = sc.language_id WHERE sc.active = 1 ORDER BY sc.created_at")
	if err != nil {
		return nil, nil, Fail(StepPostImport, "reading the sales channel access keys failed", err, "")
	}
	keys := map[string]string{}
	var order []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 4 || (f[2] != SalesChannelTypeStorefront && f[2] != SalesChannelTypeAPI) {
			continue
		}
		name := f[0]
		if _, dup := keys[name]; dup {
			name = fmt.Sprintf("%s #%d", f[0], len(order)+1)
		}
		keys[name] = f[1]
		keys[f[3]] = f[1]
		order = append(order, name)
	}
	return keys, order, nil
}

var placeholderRe = regexp.MustCompile(`\$\{(SHOP_URL|SHOP_URL_INTERNAL|FRONTEND_URL|ACCESS_KEY)(?::([^}]+))?\}`)

// FrontendEnv resolves placeholders in the frontend env definition.
func (l *Local) FrontendEnv(keys map[string]string, order []string) (map[string]string, error) {
	env := map[string]string{}
	var missing error
	for k, v := range l.Frontend.Env {
		env[k] = placeholderRe.ReplaceAllStringFunc(v, func(m string) string {
			sub := placeholderRe.FindStringSubmatch(m)
			switch sub[1] {
			case "SHOP_URL":
				return l.ShopURL()
			case "SHOP_URL_INTERNAL":
				return "http://shop"
			case "FRONTEND_URL":
				return "http://localhost:" + strconv.Itoa(l.FrontPort)
			}
			if sub[2] != "" {
				if key, ok := keys[sub[2]]; ok {
					return key
				}
				missing = &Error{Step: StepFrontend, Msg: fmt.Sprintf("frontend env %s: no active sales channel named %q", k, sub[2]),
					Hint: "${ACCESS_KEY:<name>} must name an active storefront or headless sales channel of the cloned shop (frontend.env in .dockware-clone.yml)"}
				return ""
			}
			if len(order) == 0 {
				missing = &Error{Step: StepFrontend, Msg: "frontend env " + k + ": no storefront or headless sales channel",
					Hint: "the cloned shop has no active storefront or headless sales channel to take ${ACCESS_KEY} from"}
				return ""
			}
			return keys[order[0]]
		})
	}
	return env, missing
}

// StartFrontend writes frontend.env and starts the frontend service.
func (l *Local) StartFrontend(ctx context.Context, env map[string]string) error {
	if err := os.WriteFile(filepath.Join(l.Dir, "frontend.env"), []byte(dockerEnvFile(env)), 0o600); err != nil {
		return Fail(StepFrontend, "writing frontend.env failed", err, "")
	}
	return l.compose(ctx, StepFrontend, "--profile", "frontend", "up", "-d", "frontend")
}
