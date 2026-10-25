//go:build mycel_module_development

package server

// developmentBuild is true only in a source build with the mycel_module_development tag,
// which lets a typed module listen on TCP for local development.
const developmentBuild = true
