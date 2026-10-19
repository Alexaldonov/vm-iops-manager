package main

import "testing"

func TestNormalizeDatastorePath(t *testing.T) {
	cases := []struct{ a, b string }{
		{"[Datastore1] vm1/vm1.vmx", "[datastore1] vm1/vm1.vmx"},
		{"[Datastore1]  vm1/vm1.vmx", "[datastore1] vm1/vm1.vmx"}, // двойной пробел после имени датастора / double space after the datastore name
		{"  [ds] a/b.vmdk  ", "[ds] a/b.vmdk"},
		{"[DS] A/B.VMDK", "[ds] a/b.vmdk"},
	}
	for _, c := range cases {
		if got := normalizeDatastorePath(c.a); got != c.b {
			t.Errorf("normalizeDatastorePath(%q) = %q, want %q", c.a, got, c.b)
		}
	}
}

func TestExtractDatastoreName(t *testing.T) {
	cases := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"[datastore1] vm1/vm1.vmx", "datastore1", true},
		{"[My Datastore] vm1/vm1_1.vmdk", "My Datastore", true},
		{"no-brackets-here.vmx", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := extractDatastoreName(c.path)
		if got != c.want || ok != c.wantOK {
			t.Errorf("extractDatastoreName(%q) = (%q, %v), want (%q, %v)", c.path, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSplitDatastoreRelativePath(t *testing.T) {
	cases := []struct {
		path       string
		wantFolder string
		wantFile   string
		wantOK     bool
	}{
		{"[datastore1] vm1/vm1.vmx", "vm1", "vm1.vmx", true},
		{"[datastore1] vm1/nested/vm1_1.vmdk", "vm1/nested", "vm1_1.vmdk", true},
		{"[datastore1] vm1.vmx", "", "vm1.vmx", true}, // файл в корне датастора / file at the datastore root
		{"no-brackets-here.vmx", "", "", false},
		{"[datastore1]", "", "", false},
		{"[datastore1] ", "", "", false},
	}
	for _, c := range cases {
		folder, file, ok := splitDatastoreRelativePath(c.path)
		if folder != c.wantFolder || file != c.wantFile || ok != c.wantOK {
			t.Errorf("splitDatastoreRelativePath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.path, folder, file, ok, c.wantFolder, c.wantFile, c.wantOK)
		}
	}
}

func TestLimitStr(t *testing.T) {
	if limitStr(-1) != "Unlimited" {
		t.Errorf("limitStr(-1) should be Unlimited, got %q", limitStr(-1))
	}
	if limitStr(1000) != "1000" {
		t.Errorf("limitStr(1000) = %q, want 1000", limitStr(1000))
	}
}
