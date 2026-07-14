package options

import (
	"strings"
	"time"
)

// CSV implements go-flags.Unmarshaler for comma-separated strings
type CSV []string

func (c *CSV) UnmarshalFlag(value string) error {
	if value == "" {
		*c = []string{}
		return nil
	}

	items := strings.Split(value, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
	}
	*c = items
	return nil
}

func (c *CSV) String() string {
	return strings.Join(*c, ",")
}

type Options struct {
	TFToken                     string        `short:"t" long:"terraform-token"                env:"TERRAFORM_TOKEN"                description:"Terraform API token"`
	Workspace                   string        `short:"w" long:"workspace"                      env:"TERRAFORM_WORKSPACE"            description:"Terraform Workspace"`
	Organization                string        `short:"o" long:"org"                            env:"TERRAFORM_ORGANIZATION"         description:"Terraform Organization"`
	VariableName                CSV           `short:"n" long:"variable-name"                  env:"TERRAFORM_VARIABLE_NAME"        description:"Terraform Variable Name"`
	VariableValue               CSV           `short:"v" long:"variable-value"                 env:"TERRAFORM_VARIABLE_VALUE"       description:"Value to set for the Terraform Variable named in VariableName"`
	VariablePath                CSV           `short:"p" long:"variable-path"                  env:"TERRAFORM_VARIABLE_PATH"        description:"Dotted path to a string inside an HCL variable, e.g. a.image. A * segment fans out across every key at that level. Omit to replace the whole value."`
	RunTitle                    string        `          long:"run-title"                      env:"TERRAFORM_RUN_TITLE"            description:"Title for the Terraform Run. Defaults to latest commit message if unset."`
	DryRun                      bool          `          long:"dry-run"                                                             description:"Do not actually run the Terraform Run. Useful for testing."`
	VariableValueRequiredPrefix string        `          long:"variable-value-required-prefix" env:"VARIABLE_VALUE_REQUIRED_PREFIX" description:"If set, the VariableValue must start with this prefix"`
	Timeout                     time.Duration `          long:"timeout"                        env:"TIMEOUT"                        description:"Run timeout"                                                                                                                                         default:"0"`
}
