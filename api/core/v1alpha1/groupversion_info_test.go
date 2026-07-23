package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
)

func TestGroupVersion_IsCorrect(t *testing.T) {
	assert.Equal(t, "gitops.open-control-plane.io", v1alpha1.GroupVersion.Group)
	assert.Equal(t, "v1alpha1", v1alpha1.GroupVersion.Version)
}
