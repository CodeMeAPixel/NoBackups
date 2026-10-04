package deploy

import _ "embed"

//go:embed nobackups.service
var SystemdUnit string

//go:embed nobackups.env.example
var EnvExample []byte
