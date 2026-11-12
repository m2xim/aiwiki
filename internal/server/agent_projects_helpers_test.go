package server

// AgentProjectRowForTest and AgentProjectForTest mirror the API shape of an agent project for decoding.
type AgentProjectRowForTest struct {
	RowID      string `json:"row_id"`
	Mode       string `json:"mode"`
	Accessible bool   `json:"accessible"`
	SpaceID    string `json:"space_id"`
	Name       string `json:"name"`
	MyRole     string `json:"my_role"`
	Effective  string `json:"effective"`
}

type AgentProjectForTest struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	OwnerID     string                   `json:"owner_id"`
	OwnerName   string                   `json:"owner_name"`
	Visibility  string                   `json:"visibility"`
	CanEdit     bool                     `json:"can_edit"`
	Spaces      []AgentProjectRowForTest `json:"spaces"`
}
