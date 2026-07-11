// Package provenance contains process-only proof tokens shared by trusted
// AutoSQL inspectors. Go's internal-package boundary prevents external schema
// producers from manufacturing these values.
package provenance

// GeneratedName proves that an inspector established catalog-derived naming.
// Its state is intentionally not constructible outside this package.
type GeneratedName struct{ valid bool }

// CatalogGeneratedName returns a proof for an in-repository database inspector.
func CatalogGeneratedName() GeneratedName { return GeneratedName{valid: true} }

// Valid reports whether this is a proof issued by this package.
func (proof GeneratedName) Valid() bool { return proof.valid }
