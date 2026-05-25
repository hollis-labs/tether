// Package router plans normalized AI gateway routes.
//
// The first slice intentionally stays narrow: explicit provider/model
// selection, ordered fallback across configured routes, catalog-backed
// capability checks, and simple budget/limit validation.
package router
