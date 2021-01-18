package cluster

// K0sConfig holds configuration for bootstraping k0s cluster
type K0s struct {
	Version  string      `yaml:"version"`
	Config   Mapping     `yaml:"k0s"`
	Metadata K0sMetadata `yaml:"-"`
}

// K0sMetadata contains gathered information about k0s cluster
type K0sMetadata struct {
	ClusterID string
	JoinToken string
}
