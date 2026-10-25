//go:build !mycel_module_development

package server

// developmentBuild is false in every release build: a typed module serves only on a unix
// socket (sdk.md S7, core I7).
const developmentBuild = false
