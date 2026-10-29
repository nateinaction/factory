package config_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestConfig runs the config suite.
func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "config")
}
