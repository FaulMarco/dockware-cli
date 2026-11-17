package clone

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind describes how a column is anonymised. Every kind is expressed as one SQL
// expression, so the same rule set drives both strategies: shopware-cli uses it
// as a SELECT rewrite, the shadow strategy as INSERT … SELECT expression.
type Kind string

const (
	KindEmail     Kind = "email"
	KindFirstName Kind = "first_name"
	KindLastName  Kind = "last_name"
	KindCompany   Kind = "company"
	KindStreet    Kind = "street"
	KindZip       Kind = "zip"
	KindCity      Kind = "city"
	KindPhone     Kind = "phone"
	KindIP        Kind = "ip"
	KindUsername  Kind = "username"
	KindToken     Kind = "token"
	KindPassword  Kind = "password"
	KindEmptyJSON Kind = "empty_json_array"
	// KindNull clears the value: NULL for nullable columns, '' or '{}' otherwise.
	KindNull Kind = "null"
	// KindJSONEmail replaces an address in the "email" key of a JSON column;
	// "*@domain" wildcards stay. Built in only.
	KindJSONEmail Kind = "json_email"
)

// KnownPasswordHash is the bcrypt hash of "shopware". Admin users, customers and
// integration secrets all get this value, so every account works locally with
// the dockware default password and no real hash leaves the server.
const KnownPasswordHash = "$2y$10$OLyjdGg5/EDY4Xb6mH2.eeODCPzWWOLXXKOtlXdPnfpwPkNun3HCW"

// Rule anonymises one column. Rules for columns that do not exist in the
// remote schema are skipped, so one rule set serves several Shopware versions.
type Rule struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Kind   Kind   `json:"kind"`
}

// UnmarshalYAML reads a rule from the config file. An unquoted `kind: null`
// is a YAML null, which yaml.v3 never hands to the field's own unmarshaler
// (it zeroes the field instead), so the mapping is walked here and a null
// kind becomes KindNull. Unknown keys stay an error, as with KnownFields.
func (r *Rule) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a rule is a mapping with table, column and kind", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		var dst *string
		switch key.Value {
		case "table":
			dst = &r.Table
		case "column":
			dst = &r.Column
		case "kind":
			if val.ShortTag() == "!!null" {
				r.Kind = KindNull
				continue
			}
			dst = (*string)(&r.Kind)
		default:
			return fmt.Errorf("line %d: field %s not found in rule", key.Line, key.Value)
		}
		if err := val.Decode(dst); err != nil {
			return err
		}
	}
	return nil
}

// DefaultRules covers the shopware-cli defaults plus the gaps found in the
// shopware-cli 0.19.7 review (comments, birthdays, VAT ids, secondary address
// lines, payment custom fields, admin users, integration secrets, tokens).
func DefaultRules() []Rule {
	var rules []Rule
	add := func(table string, kind Kind, columns ...string) {
		for _, c := range columns {
			rules = append(rules, Rule{Table: table, Column: c, Kind: kind})
		}
	}

	for _, t := range []string{"customer", "order_customer"} {
		add(t, KindEmail, "email")
		add(t, KindFirstName, "first_name")
		add(t, KindLastName, "last_name")
		add(t, KindCompany, "company")
		add(t, KindIP, "remote_address")
		add(t, KindNull, "title", "vat_ids", "custom_fields")
	}
	add("customer", KindPassword, "password")
	add("customer", KindNull, "birthday", "legacy_password", "legacy_encoder", "hash", "affiliate_code", "campaign_code")

	for _, t := range []string{"customer_address", "order_address"} {
		add(t, KindFirstName, "first_name")
		add(t, KindLastName, "last_name")
		add(t, KindCompany, "company")
		add(t, KindStreet, "street")
		add(t, KindZip, "zipcode")
		add(t, KindCity, "city")
		add(t, KindPhone, "phone_number")
		add(t, KindNull, "title", "department", "additional_address_line1", "additional_address_line2", "vat_id", "hash", "custom_fields")
	}

	add("order", KindNull, "customer_comment", "internal_comment", "custom_fields")
	add("order", KindToken, "deep_link_code")
	// order_line_item.payload and label stay: product data the order view needs
	add("order_line_item", KindNull, "custom_fields")
	add("order_delivery", KindEmptyJSON, "tracking_codes")
	add("order_delivery", KindNull, "custom_fields")
	add("order_transaction", KindNull, "custom_fields", "validation_data")
	add("order_transaction_capture", KindNull, "external_reference", "custom_fields")
	add("order_transaction_capture_refund", KindNull, "external_reference", "reason", "custom_fields")

	add("newsletter_recipient", KindEmail, "email")
	add("newsletter_recipient", KindFirstName, "first_name")
	add("newsletter_recipient", KindLastName, "last_name")
	add("newsletter_recipient", KindStreet, "street")
	add("newsletter_recipient", KindZip, "zip_code")
	add("newsletter_recipient", KindCity, "city")
	add("newsletter_recipient", KindToken, "hash")
	add("newsletter_recipient", KindNull, "title", "custom_fields")

	add("product_review", KindEmail, "email", "external_email")
	add("product_review", KindFirstName, "external_user")
	add("product_review", KindNull, "comment", "custom_fields")
	add("promotion_individual_code", KindNull, "payload")
	// customerEmail conditions can name single customers; Shopware rebuilds the
	// serialised rule.payload, which repeats the conditions, from rule_condition
	add("rule_condition", KindJSONEmail, "value")
	add("rule", KindNull, "payload")
	// Shopware stores the data of the last sent mail (order, customer) here
	add("mail_template_type", KindNull, "template_data")

	add("user", KindUsername, "username")
	add("user", KindEmail, "email")
	add("user", KindFirstName, "first_name")
	add("user", KindLastName, "last_name")
	add("user", KindPassword, "password")
	add("user", KindNull, "title", "store_token", "custom_fields")
	add("user_access_key", KindPassword, "secret_access_key")
	add("integration", KindPassword, "secret_access_key")
	add("app", KindToken, "app_secret")

	return rules
}

// PlaceholderEmailDomain is the domain of every rewritten e-mail address.
const PlaceholderEmailDomain = "example.com"

// personalTable reports whether a table belongs to the customer/order family,
// whose custom_fields and JSON columns are nulled unless configured otherwise.
func personalTable(table string) bool {
	t := strings.ToLower(table)
	switch t {
	case "customer", "order", "newsletter_recipient", "cart", "product_review", "mail_template_type":
		return true
	}
	return strings.HasPrefix(t, "customer_") || strings.HasPrefix(t, "order_")
}

// structuralJSONColumns of the customer/order family hold calculated prices,
// states or ids and no personal data; the admin cannot load an order without them.
var structuralJSONColumns = map[string]bool{
	"order.price": true, "order.shipping_costs": true, "order.item_rounding": true, "order.total_rounding": true, "order.rule_ids": true,
	"order_line_item.price": true, "order_line_item.price_definition": true, "order_line_item.payload": true, "order_line_item.states": true,
	"order_delivery.shipping_costs": true, "order_delivery_position.price": true,
	"order_transaction.amount": true, "order_transaction_capture.amount": true,
	"order_transaction_capture_refund.amount": true, "order_transaction_capture_refund_position.amount": true,
	"customer.tag_ids": true, "customer_specific_features.features": true,
	"mail_template_type.available_entities": true,
}

// EffectiveRules is the rule set of a clone: the defaults, a null rule for
// every custom_fields and JSON column of the customer/order family, minus
// keep_columns, with the project rules on top (a project rule replaces the
// rule for its column). Default rules for columns the source lacks are
// skipped later; project rules must name existing columns. JSON columns a
// generated column of the same table reads are never cleared by default:
// a NOT NULL generated column would make the shadow copy or the import fail.
func EffectiveRules(s Schema, cfg *ProjectConfig) ([]Rule, error) {
	if cfg == nil {
		cfg = &ProjectConfig{}
	}
	var unknown []string
	for _, r := range cfg.Rules {
		if _, ok := s[r.Table][r.Column]; !ok {
			unknown = append(unknown, r.Table+"."+r.Column)
		}
	}
	if len(unknown) > 0 {
		return nil, &Error{Step: StepConfig,
			Msg:  ".dockware-clone.yml: rules name columns the source database does not have: " + strings.Join(unknown, ", "),
			Hint: "fix or remove these rules in .dockware-clone.yml (table and column are the MySQL names); see " + DocsURL + " (configuration)"}
	}

	keep := map[string]bool{}
	for _, k := range cfg.KeepColumns {
		keep[strings.ToLower(k)] = true
	}
	var rules []Rule
	var notNull []string
	covered := map[string]bool{}
	genRefs := generationRefs(s)
	feedsGenerated := func(table, column string) bool {
		c, ok := s[table][column]
		return ok && c.DataType == "json" && genRefs[strings.ToLower(table+"."+column)]
	}
	for _, r := range DefaultRules() {
		key := strings.ToLower(r.Table + "." + r.Column)
		covered[key] = true
		if keep[key] {
			if r.Kind == KindNull {
				continue
			}
			notNull = append(notNull, key)
		}
		if r.Kind == KindNull && feedsGenerated(r.Table, r.Column) {
			continue
		}
		rules = append(rules, r)
	}
	if len(notNull) > 0 {
		return nil, &Error{Step: StepConfig,
			Msg:  ".dockware-clone.yml: keep_columns can only lift rules that clear a column, not: " + strings.Join(notNull, ", "),
			Hint: "remove these entries from keep_columns; a rules entry can change how such a column is anonymised, see " + DocsURL + " (configuration)"}
	}

	tables := make([]string, 0, len(s))
	for t := range s {
		if personalTable(t) {
			tables = append(tables, t)
		}
	}
	sort.Strings(tables)
	for _, t := range tables {
		names := make([]string, 0, len(s[t]))
		for name := range s[t] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			c := s[t][name]
			key := strings.ToLower(t + "." + name)
			if c.Generated || covered[key] || keep[key] || structuralJSONColumns[key] || feedsGenerated(t, name) {
				continue
			}
			if strings.ToLower(name) == "custom_fields" || c.DataType == "json" {
				rules = append(rules, Rule{Table: t, Column: name, Kind: KindNull})
			}
		}
	}
	return mergeRules(rules, cfg.Rules), nil
}

// generationRefs returns "table.column" (lower case) of every column that a
// generated column of the same table names in its GENERATION_EXPRESSION.
// Identifiers are matched as words, so a JSON path naming a column counts
// too; that only keeps a column raw, it never clears one.
func generationRefs(s Schema) map[string]bool {
	refs := map[string]bool{}
	for t, cols := range s {
		words := map[string]bool{}
		for _, c := range cols {
			if c.GenerationExpression == "" {
				continue
			}
			for _, w := range strings.FieldsFunc(strings.ToLower(c.GenerationExpression), func(r rune) bool {
				return !(r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
			}) {
				words[w] = true
			}
		}
		for name, c := range cols {
			if !c.Generated && words[strings.ToLower(name)] {
				refs[strings.ToLower(t+"."+name)] = true
			}
		}
	}
	return refs
}

// UnruledTextColumns returns the text and JSON columns of the given tables
// that no rule rewrites, sorted per table; tables without any are omitted.
func UnruledTextColumns(s Schema, tables []string, rules []Rule) map[string][]string {
	ruled := map[string]bool{}
	for _, r := range rules {
		ruled[r.Table+"."+r.Column] = true
	}
	out := map[string][]string{}
	for _, t := range tables {
		var cols []string
		for name, c := range s[t] {
			if isText(c.DataType) && !c.Generated && !ruled[t+"."+name] {
				cols = append(cols, name)
			}
		}
		if len(cols) > 0 {
			sort.Strings(cols)
			out[t] = cols
		}
	}
	return out
}

// NoDataTables keeps only the schema of tables whose rows are transient,
// derived or personal and not needed to run a shop locally. Glob patterns allowed.
func NoDataTables() []string {
	return []string{
		// shopware-cli --clean set
		"cart", "customer_recovery", "dead_message", "enqueue", "messenger_messages",
		"import_export_log", "increment", "elasticsearch_index_task", "admin_elasticsearch_index_task",
		"log_entry", "message_queue_stats", "notification", "payment_token", "refresh_token",
		"version", "version_commit", "version_commit_data", "webhook_event_log",
		// gaps: sessions, recovery tokens, generated documents, import files
		"sales_channel_api_context", "user_recovery", "document", "import_export_file",
		"app_flow_event", "flow_execution", "scheduled_task_execution",
		// admin UI preferences (grid filters, search history), consent decisions of admins
		"user_config", "consent_state",
		// logs of any extension
		"*_log", "*_logs", "*_log_entry",
	}
}

// Secret system_config rows are dropped; the local shop falls back to the
// defaults and projects re-add sandbox keys via post_sql. The patterns are
// matched against the lower-cased key and value, are valid in Go (RE2) as well
// as in MySQL 5.7 (Henry Spencer), MySQL 8 (ICU) and MariaDB (PCRE), and use
// no flags or \s, so the SQL filter of the dump and isSecretConfig, which
// verify runs on the imported rows, decide identically.
//
// The key list is deliberately broad: a dropped non-secret key only reverts to
// its default (e.g. core.loginRegistration.passwordMinLength). "key" counts
// only at the end of a key segment (apiKey, siteKey, publicKey), so keywords
// stays; "credential" instead of "cred" keeps credit card toggles. None of the
// words occurs in core.basicInformation.*, core.listing.* or core.cart.* keys.
const SecretConfigKeys = `(password|secret|token|apikey|api_key|accesskey|access_key|privatekey|private_key|private|clientid|client_id|merchantid|webhook|license|shopid|hmac|auth|credential|salt|signature|passphrase|dsn|username|key([._]|$))`

// SecretConfigValues finds non-empty secrets nested in a configuration_value,
// e.g. {"_value": {"clientSecret": "…"}} of plugin configs or captcha settings.
const SecretConfigValues = `"[a-z0-9_]*(secret|password|privatekey|private_key|apikey|api_key|token)[a-z0-9_]*"[[:space:]]*:[[:space:]]*"[^"]+"`

// protectedConfigKeys identify the source installation and never leave it.
var protectedConfigKeys = []string{"core.store.shopSecret", "core.app.shopId"}

var (
	secretConfigKeyRe   = regexp.MustCompile(SecretConfigKeys)
	secretConfigValueRe = regexp.MustCompile(SecretConfigValues)
)

// secretKeySQL and secretValueSQL are true for the rows SystemConfigWhere drops.
func secretKeySQL() string {
	quoted := make([]string, len(protectedConfigKeys))
	for i, k := range protectedConfigKeys {
		quoted[i] = "'" + k + "'"
	}
	return "(LOWER(configuration_key) REGEXP '" + SecretConfigKeys + "' OR configuration_key IN (" + strings.Join(quoted, ",") + "))"
}

func secretValueSQL() string {
	return "(LOWER(COALESCE(CAST(configuration_value AS CHAR), '')) REGEXP '" + SecretConfigValues + "')"
}

// isSecretConfig is the Go equivalent of the SQL filter; verify applies it to
// the imported rows as a second implementation that does not depend on MySQL.
func isSecretConfig(key, value string) bool {
	for _, k := range protectedConfigKeys {
		if key == k {
			return true
		}
	}
	return secretConfigKeyRe.MatchString(strings.ToLower(key)) || secretConfigValueRe.MatchString(strings.ToLower(value))
}

// SystemConfigWhere is the WHERE clause applied to system_config.
func SystemConfigWhere() string {
	return "NOT " + secretKeySQL() + " AND NOT " + secretValueSQL()
}

// Column is the schema metadata needed to render a rule.
type Column struct {
	Table                string
	Name                 string
	Nullable             bool
	DataType             string
	Generated            bool
	GenerationExpression string // of a generated column, as information_schema reports it
}

// Schema maps table → column name → metadata.
type Schema map[string]map[string]Column

// HasID reports whether the table has a binary id column usable as seed.
func (s Schema) HasID(table string) bool {
	c, ok := s[table]["id"]
	return ok && (c.DataType == "binary" || c.DataType == "varbinary")
}

// Applicable returns the rules whose table and column exist, sorted.
func Applicable(rules []Rule, schema Schema) []Rule {
	var out []Rule
	for _, r := range rules {
		cols, ok := schema[r.Table]
		if !ok {
			continue
		}
		c, ok := cols[r.Column]
		if !ok || c.Generated {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Column < out[j].Column
	})
	return out
}

var (
	firstNames = []string{"Anna", "Ben", "Clara", "David", "Emma", "Felix", "Greta", "Hannes", "Ida", "Jonas", "Lena", "Max", "Nora", "Paul", "Sophie", "Tom"}
	lastNames  = []string{"Muster", "Beispiel", "Schmidt", "Weber", "Fischer", "Wagner", "Becker", "Hoffmann", "Koch", "Richter", "Klein", "Wolf"}
	cities     = []string{"Berlin", "Hamburg", "München", "Köln", "Leipzig", "Bremen", "Dresden", "Freiburg"}
)

func elt(seed string, values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return fmt.Sprintf("ELT(1 + %s %% %d, %s)", seed, len(values), strings.Join(quoted, ","))
}

// Expression renders the SQL rewrite for a rule. Columns that are NULL or empty
// stay as they are, so optional fields keep their shape and no value is invented.
func Expression(r Rule, schema Schema) string {
	col := schema[r.Table][r.Column]
	q := "`" + r.Column + "`"

	hexID := "LOWER(HEX(`id`))"
	crc := "CRC32(`id`)"
	if !schema.HasID(r.Table) {
		hexID = "LOWER(REPLACE(UUID(),'-',''))"
		crc = "CRC32(UUID())"
	}

	email := "CONCAT('" + strings.ReplaceAll(r.Table, "_", "-") + "-', " + hexID + ", '@" + PlaceholderEmailDomain + "')"
	var expr string
	switch r.Kind {
	case KindEmail:
		expr = email
	case KindFirstName:
		expr = elt(crc, firstNames)
	case KindLastName:
		expr = elt("("+crc+" DIV 16)", lastNames)
	case KindCompany:
		// UUIDv7 ids share their time prefix: take the random tail
		expr = "CONCAT('Firma ', UPPER(RIGHT(" + hexID + ", 6)))"
	case KindStreet:
		expr = "CONCAT('Musterstraße ', 1 + " + crc + " % 200)"
	case KindZip:
		expr = "LPAD(" + crc + " % 100000, 5, '0')"
	case KindCity:
		expr = elt("("+crc+" DIV 256)", cities)
	case KindPhone:
		expr = "CONCAT('+49 30 ', LPAD(" + crc + " % 10000000, 7, '0'))"
	case KindIP:
		expr = "'127.0.0.1'"
	case KindUsername:
		// full id: user.username is unique and UUIDv7 prefixes collide
		expr = "CONCAT('user-', " + hexID + ")"
	case KindToken:
		expr = "LOWER(REPLACE(UUID(),'-',''))"
	case KindPassword:
		expr = "'" + KnownPasswordHash + "'"
	case KindEmptyJSON:
		expr = "'[]'"
	case KindNull:
		return nullExpression(col)
	case KindJSONEmail:
		v := "JSON_UNQUOTE(JSON_EXTRACT(" + q + ", '$.email'))"
		return "IF(" + v + " LIKE '%@%' AND " + v + " NOT LIKE '*@%', JSON_SET(" + q + ", '$.email', " + email + "), " + q + ")"
	default:
		panic("unknown anonymisation kind " + string(r.Kind))
	}
	return "IF(" + q + " IS NULL OR " + q + " = '', " + q + ", " + expr + ")"
}

func nullExpression(c Column) string {
	if c.Nullable {
		return "NULL"
	}
	if c.DataType == "json" {
		return "'{}'"
	}
	return "''"
}

// RewriteMap returns table → column → SQL for the applicable rules.
func RewriteMap(rules []Rule, schema Schema) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, r := range Applicable(rules, schema) {
		if out[r.Table] == nil {
			out[r.Table] = map[string]string{}
		}
		out[r.Table][r.Column] = Expression(r, schema)
	}
	return out
}

// RulesHash fingerprints the effective rule set for the audit log.
func RulesHash(rewrites map[string]map[string]string, noData []string, where map[string]string) string {
	h := sha256.New()
	tables := make([]string, 0, len(rewrites))
	for t := range rewrites {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		cols := make([]string, 0, len(rewrites[t]))
		for c := range rewrites[t] {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		for _, c := range cols {
			fmt.Fprintf(h, "r %s.%s=%s\n", t, c, rewrites[t][c])
		}
	}
	nd := append([]string(nil), noData...)
	sort.Strings(nd)
	for _, t := range nd {
		fmt.Fprintf(h, "n %s\n", t)
	}
	wk := make([]string, 0, len(where))
	for t := range where {
		wk = append(wk, t)
	}
	sort.Strings(wk)
	for _, t := range wk {
		fmt.Fprintf(h, "w %s=%s\n", t, where[t])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
