package turbine

import "errors"

var (
	ErrEmptyEntryBatch       = errors.New("entry batch cannot be empty")
	ErrInvalidBlockComponent = errors.New("invalid block component")
	ErrUnknownMarkerKind     = errors.New("unknown block marker kind")
)
