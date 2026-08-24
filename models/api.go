package models

// UpdateSplitRequest is the JSON body accepted by PATCH /splits/{id}. Fields
// are pointers so that omitted fields are left unchanged.
type UpdateSplitRequest struct {
	Start          *float64 `json:"start"`
	End            *float64 `json:"end"`
	Classification *string  `json:"classification"`
	// CustomTitle is the display-only rename for the split. "title" is
	// accepted as an alias for backward compatibility.
	CustomTitle *string `json:"custom_title"`
	Title       *string `json:"title"`
}