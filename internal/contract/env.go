// Package contract implements the runner's side of the Container Driver contract: environment, outputs and the error file.
package contract

import "fmt"

// Set by the Container Driver.
const (
	EnvAction = "ACTION"
	// EnvScriptsDirectory is the shared directory; relative paths resolve against it.
	EnvScriptsDirectory = "SCRIPTS_DIRECTORY"
	EnvOutputsFile      = "OUTPUTS_FILE"
	// EnvErrorFile is where the error message is appended on failure.
	EnvErrorFile = "ERROR_FILE"
)

// Set by the Resource Definition as Job variables.
const (
	EnvBucketName = "BUCKET_NAME"
	EnvAWSRegion  = "AWS_REGION"
	// EnvAWSCredentialsFile is the optional path of a JSON file {"access_key_id","secret_access_key"}.
	EnvAWSCredentialsFile = "AWS_CREDENTIALS_FILE"
)

// Action is what the Container Driver asks the runner to do.
type Action string

// Supported actions.
const (
	ActionCreate  Action = "create"
	ActionDestroy Action = "destroy"
)

// ParseAction validates s as an Action.
func ParseAction(s string) (Action, error) {
	switch a := Action(s); a {
	case ActionCreate, ActionDestroy:
		return a, nil
	case "":
		return "", fmt.Errorf("no action: set %s to %q or %q", EnvAction, ActionCreate, ActionDestroy)
	default:
		return "", fmt.Errorf("unsupported action %q, expected %q or %q", s, ActionCreate, ActionDestroy)
	}
}
