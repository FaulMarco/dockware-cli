package clone

import (
	"errors"
	"strings"
	"testing"
)

func effectiveSchema() Schema {
	col := func(t, name, dt string) Column { return Column{Table: t, Name: name, DataType: dt, Nullable: true} }
	return Schema{
		"order": {
			"id":            col("order", "id", "binary"),
			"price":         col("order", "price", "json"),
			"custom_fields": col("order", "custom_fields", "json"),
			"acme_extra":    col("order", "acme_extra", "json"),
		},
		"order_line_item": {
			"payload":       col("order_line_item", "payload", "json"),
			"label":         col("order_line_item", "label", "varchar"),
			"custom_fields": col("order_line_item", "custom_fields", "json"),
		},
		"customer_acme": {
			"custom_fields": col("customer_acme", "custom_fields", "longtext"),
			"settings":      col("customer_acme", "settings", "json"),
			"note":          col("customer_acme", "note", "varchar"),
		},
		"product": {
			"custom_fields": col("product", "custom_fields", "json"),
		},
	}
}

func ruleKinds(rules []Rule) map[string]Kind {
	m := map[string]Kind{}
	for _, r := range rules {
		m[r.Table+"."+r.Column] = r.Kind
	}
	return m
}

func TestEffectiveRulesNullsPersonalJSONColumns(t *testing.T) {
	rules, err := EffectiveRules(effectiveSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	k := ruleKinds(rules)
	for _, c := range []string{"order.acme_extra", "order.custom_fields", "order_line_item.custom_fields", "customer_acme.custom_fields", "customer_acme.settings"} {
		if k[c] != KindNull {
			t.Errorf("%s: kind %q, want null", c, k[c])
		}
	}
	for _, c := range []string{"order.price", "order_line_item.payload", "order_line_item.label", "customer_acme.note", "product.custom_fields"} {
		if kind, ok := k[c]; ok {
			t.Errorf("%s must stay raw, got rule %q", c, kind)
		}
	}
	if k["customer.email"] != KindEmail {
		t.Errorf("default rules missing: customer.email = %q", k["customer.email"])
	}
}

func TestEffectiveRulesKeepColumnsAndConfigRules(t *testing.T) {
	cfg := &ProjectConfig{
		KeepColumns: []string{"order.acme_extra", "Order_Line_Item.Custom_Fields"},
		Rules:       []Rule{{Table: "customer_acme", Column: "settings", Kind: KindEmptyJSON}, {Table: "customer_acme", Column: "note", Kind: KindCompany}},
	}
	rules, err := EffectiveRules(effectiveSchema(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	k := ruleKinds(rules)
	if _, ok := k["order.acme_extra"]; ok {
		t.Error("order.acme_extra is in keep_columns but has a rule")
	}
	if _, ok := k["order_line_item.custom_fields"]; ok {
		t.Error("keep_columns must lift the default rule of order_line_item.custom_fields")
	}
	if k["customer_acme.settings"] != KindEmptyJSON || k["customer_acme.note"] != KindCompany {
		t.Errorf("config rules not applied: %v", k)
	}
	n := 0
	for _, r := range rules {
		if r.Table == "customer_acme" && r.Column == "settings" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("customer_acme.settings has %d rules, want 1", n)
	}
}

func TestEffectiveRulesRejectsUnknownConfigColumns(t *testing.T) {
	cfg := &ProjectConfig{Rules: []Rule{{Table: "order", Column: "nope", Kind: KindNull}, {Table: "acme_missing", Column: "mail", Kind: KindEmail}}}
	_, err := EffectiveRules(effectiveSchema(), cfg)
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepConfig {
		t.Fatalf("want StepConfig *Error, got %v", err)
	}
	if !strings.Contains(ce.Msg, "order.nope") || !strings.Contains(ce.Msg, "acme_missing.mail") || !strings.Contains(ce.Hint, ".dockware-clone.yml") {
		t.Errorf("error does not name the rules and the file: %v", err)
	}
}

func TestEffectiveRulesKeepColumnsCannotLiftRewrites(t *testing.T) {
	_, err := EffectiveRules(effectiveSchema(), &ProjectConfig{KeepColumns: []string{"customer.email"}})
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepConfig || !strings.Contains(ce.Msg, "customer.email") {
		t.Fatalf("want StepConfig *Error naming customer.email, got %v", err)
	}
}

func TestUnruledTextColumns(t *testing.T) {
	s := effectiveSchema()
	got := UnruledTextColumns(s, []string{"customer_acme", "order"}, []Rule{{Table: "customer_acme", Column: "settings", Kind: KindNull}})
	if strings.Join(got["customer_acme"], ",") != "custom_fields,note" {
		t.Errorf("customer_acme: %v", got["customer_acme"])
	}
	if strings.Join(got["order"], ",") != "acme_extra,custom_fields,price" {
		t.Errorf("order: %v", got["order"])
	}
	if _, ok := got["product"]; ok {
		t.Error("product was not requested")
	}
}

func TestSecretConfigDecision(t *testing.T) {
	cases := []struct {
		key, value string
		secret     bool
	}{
		{"core.basicInformation.shopName", `{"_value": "Demo"}`, false},
		{"core.basicInformation.email", `{"_value": "shop@example.org"}`, false},
		{"core.basicInformation.metaKeywords", `{"_value": "a"}`, false},
		{"core.listing.productsPerPage", `{"_value": 24}`, false},
		{"core.cart.maxQuantity", `{"_value": 100}`, false},
		{"core.basicInformation.activeCaptchasV2", `{"_value": {"googleReCaptchaV2": {"config": {"siteKey": "", "secretKey": ""}}}}`, false},
		{"core.basicInformation.activeCaptchasV2", `{"_value": {"googleReCaptchaV2": {"config": {"siteKey": "x", "secretKey": "abc"}}}}`, true},
		{"core.mailerSettings.password", `{"_value": "x"}`, true},
		{"core.mailerSettings.username", `{"_value": "x"}`, true},
		{"core.store.shopSecret", `{"_value": "x"}`, true},
		{"core.app.shopId", `{"_value": "x"}`, true},
		{"SwagPayPal.settings.clientSecret", `{"_value": "x"}`, true},
		{"Acme.config.apiKey", `{"_value": "x"}`, true},
		{"Acme.config.publicKey", `{"_value": "x"}`, true},
		{"Acme.config.hmacSecret", `{"_value": "x"}`, true},
		{"Acme.config.oauthMode", `{"_value": "live"}`, true},
		{"Acme.config.mailerDsn", `{"_value": "smtp://x"}`, true},
		{"Acme.config.settings", `{"_value": {"Private_Key": "-----BEGIN"}}`, true},
		{"Acme.config.settings", `{"_value": {"accessToken":"t"}}`, true},
		{"Acme.config.creditCardEnabled", `{"_value": true}`, false},
	}
	for _, c := range cases {
		if got := isSecretConfig(c.key, c.value); got != c.secret {
			t.Errorf("isSecretConfig(%q, %q) = %v, want %v", c.key, c.value, got, c.secret)
		}
	}
}

// A backslash or single quote would be consumed or end the MySQL string
// literal, so MySQL would match another pattern than isSecretConfig.
func TestSecretPatternsSurviveSQLStringLiteral(t *testing.T) {
	for _, p := range []string{SecretConfigKeys, SecretConfigValues} {
		if strings.ContainsAny(p, `'\`) {
			t.Errorf("pattern contains a quote or backslash: %s", p)
		}
	}
}

func TestEffectiveRulesKeepJSONReadByGeneratedColumns(t *testing.T) {
	s := effectiveSchema()
	s["order"]["custom_fields_flag"] = Column{Table: "order", Name: "custom_fields_flag", DataType: "tinyint", Generated: true,
		GenerationExpression: "json_unquote(json_extract(`custom_fields`,_utf8mb4'$.flag'))"}
	s["customer_acme"]["settings_mode"] = Column{Table: "customer_acme", Name: "settings_mode", DataType: "varchar", Generated: true,
		GenerationExpression: "JSON_VALUE(Settings, '$.mode')"}
	rules, err := EffectiveRules(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	k := ruleKinds(rules)
	for _, c := range []string{"order.custom_fields", "customer_acme.settings", "order.custom_fields_flag", "customer_acme.settings_mode"} {
		if kind, ok := k[c]; ok {
			t.Errorf("%s feeds or is a generated column, got rule %q", c, kind)
		}
	}
	for _, c := range []string{"order.acme_extra", "customer_acme.custom_fields", "order_line_item.custom_fields"} {
		if k[c] != KindNull {
			t.Errorf("%s: kind %q, want null", c, k[c])
		}
	}
}
