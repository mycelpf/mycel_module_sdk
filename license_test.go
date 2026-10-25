package modulesdk

import (
	"errors"
	"testing"
)

// SDK-T6 (sdk.md S2, license §5.2 and T12), build-validation half: unknown, repeated,
// empty and out-of-order product IDs are refused; an empty list means no license.
func TestValidateNeededMycelLicense(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		want error
	}{
		{"none", nil, nil},
		{"one", []string{"clc"}, nil},
		{"two in catalog order", []string{"clc", "helix"}, nil},
		{"unknown", []string{"cluster_console"}, ErrUnknownProduct},
		{"duplicate", []string{"clc", "clc"}, ErrDuplicateProduct},
		{"empty ID", []string{""}, ErrUnknownProduct},
		{"case differs", []string{"CLC"}, ErrUnknownProduct},
		{"out of catalog order", []string{"helix", "clc"}, ErrProductOrder},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateNeededMycelLicense(c.ids)
			if c.want == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestLicenseProductIDsIsACopyOfTheCatalog(t *testing.T) {
	ids := LicenseProductIDs()
	if len(ids) != 2 || ids[0] != "clc" || ids[1] != "helix" {
		t.Fatalf("catalog = %v, want [clc helix]", ids)
	}
	ids[0] = "changed"
	if LicenseProductIDs()[0] != "clc" {
		t.Fatal("the catalog can be changed through a returned slice")
	}
}

func TestLicenseLabelValue(t *testing.T) {
	if got := LicenseLabelValue([]string{"clc", "helix"}); got != "clc,helix" {
		t.Errorf("label = %q, want clc,helix", got)
	}
	if got := LicenseLabelValue(nil); got != "" {
		t.Errorf("label for no products = %q, want empty", got)
	}
}

func TestDescriptorValidationRefusesABadLicenseDeclaration(t *testing.T) {
	d := Descriptor{Token: "clc", NEEDED_MYCEL_LICENSE: []string{"clc", "clc"}}
	if err := ValidateDiscovery(d.Discovery()); !errors.Is(err, ErrDuplicateProduct) {
		t.Fatalf("error = %v, want duplicate product", err)
	}
}
