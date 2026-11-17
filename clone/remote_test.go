package clone

import (
	"strings"
	"testing"
)

func TestSSHArgsShareTheConnectionUnlessTheUserConfiguresIt(t *testing.T) {
	// the user's options come first, as ssh keeps the first value of an option
	r := Remote{Target: "shop", Extra: []string{"-o", "ProxyJump=bastion"}, ControlDir: "/tmp/dwc-ssh-1"}
	want := "-o ProxyJump=bastion -o BatchMode=yes -o ConnectTimeout=60 -o ServerAliveInterval=30 -o ControlMaster=auto -o ControlPath=/tmp/dwc-ssh-1/cm -o ControlPersist=30m"
	if got := strings.Join(r.SSHArgs(), " "); got != want {
		t.Fatalf("ssh args\n got %s\nwant %s", got, want)
	}
	for _, opt := range []string{"ControlMaster=no", "controlpath ~/.ssh/cm-%C"} {
		r := Remote{Target: "shop", Extra: []string{"-o", opt}}
		done, err := r.ShareConnection()
		if err != nil {
			t.Fatal(err)
		}
		done()
		if r.ControlDir != "" || strings.Contains(strings.Join(r.SSHArgs(), " "), "ControlMaster=auto") {
			t.Errorf("--ssh-option %q must replace the shared connection: %q", opt, r.SSHArgs())
		}
	}
}

func TestTargetArgsEndOptionsBeforeTheTarget(t *testing.T) {
	r := Remote{Target: "deploy@shop", Port: 2222}
	for _, opts := range [][]string{nil, {"-O", "exit"}, {"-G"}} {
		args := r.targetArgs(opts...)
		n := len(args)
		if n < 2 || args[n-2] != "--" || args[n-1] != "deploy@shop" {
			t.Errorf("targetArgs(%q) = %q, want ... -- deploy@shop", opts, args)
		}
		if len(opts) > 0 && args[n-2-len(opts)] != opts[0] {
			t.Errorf("targetArgs(%q) = %q: options must precede --", opts, args)
		}
	}
}

func TestParseTargetRejectsOptionLikeTargets(t *testing.T) {
	for _, target := range []string{"-oProxyCommand=sh", "ssh://-oProxyCommand=x@host"} {
		if _, err := ParseTarget(target); err == nil {
			t.Errorf("ParseTarget(%q) must fail", target)
		}
	}
	if r, err := ParseTarget("ssh://deploy@shop.example:2222"); err != nil || r.Target != "deploy@shop.example" || r.Port != 2222 {
		t.Errorf("ParseTarget ssh url = %+v, %v", r, err)
	}
}

func TestValidateSSHOptions(t *testing.T) {
	for _, tc := range []struct {
		opts []string
		bad  bool
	}{
		{nil, false},
		{[]string{"ProxyJump=bastion", "Port=2222"}, false},
		{[]string{"StrictHostKeyChecking=yes"}, false},
		{[]string{"StrictHostKeyChecking=accept-new"}, false},
		{[]string{"UserKnownHostsFile=~/.ssh/known_hosts_shop"}, false},
		{[]string{"StrictHostKeyChecking=no"}, true},
		{[]string{"stricthostkeychecking=OFF"}, true},
		{[]string{"StrictHostKeyChecking no"}, true},
		{[]string{"-o", "StrictHostKeyChecking=no"}, true},
		{[]string{"-oStrictHostKeyChecking=no"}, true},
		{[]string{"UserKnownHostsFile=/dev/null"}, true},
		{[]string{"userknownhostsfile /dev/null"}, true},
	} {
		err := ValidateSSHOptions(tc.opts)
		if (err != nil) != tc.bad {
			t.Errorf("ValidateSSHOptions(%q) = %v, want error %v", tc.opts, err, tc.bad)
			continue
		}
		if err != nil {
			if ce, ok := err.(*Error); !ok || ce.Step != StepConfig {
				t.Errorf("ValidateSSHOptions(%q) = %#v, want *Error for StepConfig", tc.opts, err)
			}
		}
	}
}

func TestParseSSHConfigHost(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"user deploy\nhostname shop.example\nport 22\n", "shop.example"},
		{"hostname 10.0.0.5\nport 2222\n", "10.0.0.5:2222"},
		{"hostname ::1\nport 2222\n", "[::1]:2222"},
		{"", ""},
	} {
		if got := parseSSHConfigHost(tc.out); got != tc.want {
			t.Errorf("parseSSHConfigHost(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

func TestDBHostPortOmitsTheDefaultPort(t *testing.T) {
	if got := dbHostPort("db.internal", "3306"); got != "db.internal" {
		t.Errorf("default port: %q", got)
	}
	if got := dbHostPort("127.0.0.1", "3307"); got != "127.0.0.1:3307" {
		t.Errorf("custom port: %q", got)
	}
}
