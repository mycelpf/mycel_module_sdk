package modulesdk

import (
	"errors"
	"fmt"
	"strings"
)

// LicenseProductIDs returns the license product catalog in catalog order (license E14,
// §5.1). This function is the SDK's only copy of the catalog. It matches the catalog of
// mycel_license (github.com/cogniworks/mycel_license/catalog) until the SDK can consume
// that catalog directly; a new product needs a change here and there.
func LicenseProductIDs() []string {
	return []string{"clc", "helix"}
}

// Errors from ValidateNeededMycelLicense. Each is wrapped with the offending value.
var (
	ErrUnknownProduct   = errors.New("product ID is not in the license catalog")
	ErrDuplicateProduct = errors.New("duplicate product ID")
	ErrProductOrder     = errors.New("product IDs are not in catalog order")
)

// ValidateNeededMycelLicense checks a NEEDED_MYCEL_LICENSE list (license §5.2): every ID
// must be in the catalog exactly as spelled there, appear once, and be listed in catalog
// order, so the list, the Describe field and the image label are one canonical value.
// An empty list is valid and means no license is needed.
func ValidateNeededMycelLicense(ids []string) error {
	catalog := LicenseProductIDs()
	position := make(map[string]int, len(catalog))
	for index, id := range catalog {
		position[id] = index
	}
	seen := make(map[string]bool, len(ids))
	last := -1
	for _, id := range ids {
		at, known := position[id]
		if !known {
			return fmt.Errorf("NEEDED_MYCEL_LICENSE: %w: %q (catalog: %s)", ErrUnknownProduct, id, strings.Join(catalog, ", "))
		}
		if seen[id] {
			return fmt.Errorf("NEEDED_MYCEL_LICENSE: %w: %q", ErrDuplicateProduct, id)
		}
		seen[id] = true
		if at < last {
			return fmt.Errorf("NEEDED_MYCEL_LICENSE: %w: %q, want order %q", ErrProductOrder, ids, catalog)
		}
		last = at
	}
	return nil
}

// LicenseLabelValue is the value of the image label io.mycel.licensing.product for a
// validated product list: the IDs comma-separated in catalog order (license §5.2). An
// empty list gives "", meaning the label is left off the image.
func LicenseLabelValue(ids []string) string {
	return strings.Join(ids, ",")
}
