// Package i18n manages the bilingual (Chinese / English) message bundles
// used by the udf CLI. Library functions wrap user-facing failures in
// *LocalizedError, which carries a stable key and renders a readable
// default message in English.
package i18n
