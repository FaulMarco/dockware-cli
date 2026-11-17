package clone

import (
	"strconv"
	"strings"
	"testing"
)

func TestBatchesKeepEveryQueryOnce(t *testing.T) {
	items := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, strconv.Itoa(i))
	}
	cases := []struct {
		n, size int
		want    string
	}{
		{0, 2, ""},
		{1, 200, "0"},
		{4, 2, "0,1|2,3"},
		{5, 2, "0,1|2,3|4"},
	}
	for _, c := range cases {
		var parts []string
		for _, b := range batches(items[:c.n], c.size) {
			parts = append(parts, strings.Join(b, ","))
		}
		if got := strings.Join(parts, "|"); got != c.want {
			t.Errorf("batches(%d items, %d) = %q, want %q", c.n, c.size, got, c.want)
		}
	}
}

func TestEmailExpected(t *testing.T) {
	extra := []string{"acme_*.contact", "customer.acme_shop_mail"}
	cases := []struct {
		column string
		want   bool
	}{
		{"system_config.configuration_value", true},
		{"system_config.configuration_key", false},
		{"mail_template_translation.content_html", true},
		{"mail_header_footer_translation.footer_html", true},
		{"sales_channel_domain.url", true},
		{"cms_slot_translation.config", true},
		{"category_translation.slot_config", true},
		{"category_translation.description", true},
		{"category_translation.custom_fields", false},
		{"landing_page_translation.slot_config", true},
		{"flow_sequence.config", true},
		{"event_action.config", true},
		{"event_action.custom_fields", false},
		{"swag_cms_extensions_form_translation.receivers", true},
		{"user_config.value", false},
		{"product_translation.description", true},
		{"snippet.value", true},
		{"app_translation.privacy_policy_extensions", true},
		{"acme_dealer.contact", true},
		{"acme_dealer.notes", false},
		{"customer.acme_shop_mail", true},
		{"customer.email", false},
		{"order_address.street", false},
		{"product_review.content", false},
		{"newsletter_recipient.email", false},
		{"user.email", false},
		{"user.title", false},
		{"rule_condition.value", false},
	}
	for _, c := range cases {
		if got := emailExpected(c.column, extra); got != c.want {
			t.Errorf("emailExpected(%q) = %v, want %v", c.column, got, c.want)
		}
	}
}

// A kind without a verify pattern fails the shape check on every clone.
func TestEveryDefaultRuleHasVerifyPattern(t *testing.T) {
	for _, r := range DefaultRules() {
		if localPattern(r, "`c`") == "0" {
			t.Errorf("%s.%s: no verify pattern for kind %q", r.Table, r.Column, r.Kind)
		}
	}
}

func TestDifferingSalesChannelIDs(t *testing.T) {
	a := lineSet("0a\n0b\n\n")
	if n := differing(a, lineSet("0b\n0a")); n != 0 {
		t.Errorf("same ids differ by %d", n)
	}
	if n := differing(a, lineSet("0a\n0c")); n != 2 {
		t.Errorf("one swapped id: %d, want 2", n)
	}
}

func TestSecretConfigRowsReadsBatchOutput(t *testing.T) {
	out := "core.basicInformation.shopName\t{\"_value\": \"Demo\\\\Shop\"}\n" +
		"core.mailerSettings.password\t{\"_value\": \"x\"}\n" +
		"Acme.config.settings\t{\"_value\": {\\n\\t\"clientSecret\":\\t\"abc\"}}\n" +
		"core.listing.productsPerPage\t{\"_value\": 24}\n" +
		"core.app.shopId\t\n"
	got := strings.Join(secretConfigRows(out), ",")
	if got != "Acme.config.settings,core.app.shopId,core.mailerSettings.password" {
		t.Errorf("secretConfigRows = %q", got)
	}
	if s := unescapeBatch(`a\\b\tc\nd\0`); s != "a\\b\tc\nd\x00" {
		t.Errorf("unescapeBatch = %q", s)
	}
}
