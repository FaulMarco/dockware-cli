package clone

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

// Audit is written to clone.log.json: who cloned what, how, and the proof
// that the copy is anonymised. It never contains row data or secrets.
type Audit struct {
	User               string                   `json:"user"`
	Host               string                   `json:"host"`
	Started            time.Time                `json:"started"`
	Finished           time.Time                `json:"finished"`
	Name               string                   `json:"name"`
	OutDir             string                   `json:"out_dir"`
	Target             string                   `json:"target"`
	Root               string                   `json:"root"`
	ResolvedHost       string                   `json:"resolved_host,omitempty"`
	Database           string                   `json:"database,omitempty"`
	DBHost             string                   `json:"db_host,omitempty"`
	AppURL             string                   `json:"app_url,omitempty"`
	ExpiresAt          time.Time                `json:"expires_at"`
	Verified           bool                     `json:"verified"`
	Wiped              bool                     `json:"wiped,omitempty"`
	Strategy           string                   `json:"strategy"`
	ShopwareCLIVersion string                   `json:"shopware_cli_version,omitempty"`
	ShopwareVersion    string                   `json:"shopware_version"`
	PHPVersion         string                   `json:"php_version"`
	NodeVersion        string                   `json:"node_version,omitempty"`
	MySQLVersion       string                   `json:"mysql_version"`
	RulesHash          string                   `json:"rules_hash"`
	Rewrites           int                      `json:"rewritten_columns"`
	Tables             Classification           `json:"tables"`
	DeniedRows         map[string]int64         `json:"denied_estimated_rows,omitempty"`
	KeptOnPurpose      []string                 `json:"kept_on_purpose"`
	OrdersSinceDays    int                      `json:"orders_since_days,omitempty"`
	OrdersCutoff       string                   `json:"orders_cutoff,omitempty"` // first order date in the dump (UTC)
	SkipSearchIndex    bool                     `json:"skip_search_index,omitempty"`
	CodeExclude        []string                 `json:"code_exclude,omitempty"`
	CodeSynced         bool                     `json:"code_synced,omitempty"` // <out>/code was synced by a clone, so purge removes it
	Dump               *DumpResult              `json:"dump,omitempty"`
	Phases             map[string]time.Duration `json:"phases_ns"`
	Checks             []Check                  `json:"checks"`
	UnsetEnv           []string                 `json:"unset_env,omitempty"`
	Warnings           []string                 `json:"warnings,omitempty"`
	Error              string                   `json:"error,omitempty"`
	ErrorStep          string                   `json:"error_step,omitempty"`
	ErrorHint          string                   `json:"error_hint,omitempty"`
}

// keptOnPurpose documents data that intentionally stays unchanged.
var keptOnPurpose = []string{
	"order.order_number, customer.customer_number (no personal data, needed to reproduce tickets)",
	"order_line_item label/payload (product data)",
	"orders, carts totals and product data",
}

func newAudit(target, root, strategy string) *Audit {
	a := &Audit{Started: time.Now(), Target: target, Root: root, Strategy: strategy, Phases: map[string]time.Duration{}, KeptOnPurpose: keptOnPurpose}
	if u, err := user.Current(); err == nil {
		a.User = u.Username
	}
	a.Host, _ = os.Hostname()
	return a
}

func (a *Audit) write(dir string) error {
	a.Finished = time.Now()
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "clone.log.json"), data, 0o600)
}

// readAudit reads the clone.log.json of a previous run in dir; nil without error when there is none.
func readAudit(dir string) (*Audit, error) {
	data, err := os.ReadFile(filepath.Join(dir, "clone.log.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := &Audit{}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, err
	}
	return a, nil
}
