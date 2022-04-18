package build

import "github.com/outofforest/buildgo"

// Commands is a definition of commands available in build system
var Commands = map[string]interface{}{
	"setup": setup,
	"build": buildApp,
}

func init() {
	buildgo.AddCommands(Commands)
}
