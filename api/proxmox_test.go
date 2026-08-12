package main

import (
	"os"
	"reflect"
	"testing"
)

func TestExtractProxmoxIdentity(t *testing.T) {
	config := map[string]any{
		"smbios1": "uuid=AA11BB22-CC33-DD44-EE55-FF6677889900",
		"net0":    "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0",
		"net1":    "name=eth1,bridge=vmbr1,hwaddr=02:00:00:00:00:01",
	}
	if got, want := extractDMIUUID(config), "aa11bb22-cc33-dd44-ee55-ff6677889900"; got != want {
		t.Fatalf("DMI UUID = %q, want %q", got, want)
	}
	got := extractProxmoxMACs(config)
	want := []string{"02:00:00:00:00:01", "bc:24:11:aa:bb:cc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MACs = %#v, want %#v", got, want)
	}
}

func TestSecretRoundTrip(t *testing.T) {
	old := os.Getenv("AXIOM_SECRET_KEY")
	t.Cleanup(func() { _ = os.Setenv("AXIOM_SECRET_KEY", old) })
	_ = os.Setenv("AXIOM_SECRET_KEY", "test-key-for-axiom-proxmox-integration")
	sealed, err := sealAxiomSecret("super-secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if sealed == "super-secret-token" || sealed == "" {
		t.Fatalf("secret was not sealed")
	}
	opened, err := openAxiomSecret(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened != "super-secret-token" {
		t.Fatalf("opened secret = %q", opened)
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"BC-24-11-AA-BB-CC": "bc:24:11:aa:bb:cc",
		"bc:24:11:aa:bb:cc": "bc:24:11:aa:bb:cc",
		"not-a-mac":         "",
	}
	for input, want := range cases {
		if got := normalizeMAC(input); got != want {
			t.Errorf("normalizeMAC(%q)=%q want %q", input, got, want)
		}
	}
}
