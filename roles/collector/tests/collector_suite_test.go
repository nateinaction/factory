package tests_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCollectorRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "collector role Suite")
}
