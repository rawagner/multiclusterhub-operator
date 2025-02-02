// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package templates

import (
	"os"
	"path"
	"testing"
)

func TestGetCoreTemplates(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working dir %v", err)
	}
	templatesPath := path.Join(path.Dir(path.Dir(path.Dir(wd))), "pkg", "templates")
	os.Setenv(TemplatesPathEnvVar, templatesPath)
	defer os.Unsetenv(TemplatesPathEnvVar)

	_, err = GetTemplateRenderer().GetTemplates()

	if err != nil {
		t.Fatalf("failed to render core template %v", err)
	}
}
