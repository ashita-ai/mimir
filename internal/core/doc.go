// Package core defines Mimir's domain types. It contains no I/O and no
// external dependencies beyond the standard library and google/uuid.
//
// Every other internal package depends on core; core depends on nothing
// inside the module. Keep it that way.
package core
