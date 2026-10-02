package domain

// ImportSource records which batch import introduced an entity and on
// which CSV line the entity was declared. It powers idempotent re-uploads
// and crash recovery reconciliation.
type ImportSource struct {
	ImportID string `json:"import_id"`
	Line     int    `json:"line"`
}
