package util

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

type cachingVerifier struct {
	cache map[schema.GroupVersionKind]error
	next  resource.Verifier
}

func newCachingVerifier(next resource.Verifier) *cachingVerifier {
	return &cachingVerifier{
		cache: make(map[schema.GroupVersionKind]error),
		next:  next,
	}
}

func (cv *cachingVerifier) HasSupport(gvk schema.GroupVersionKind) error {
	if err, ok := cv.cache[gvk]; ok {
		return err
	}
	err := cv.next.HasSupport(gvk)
	cv.cache[gvk] = err
	return err
}
