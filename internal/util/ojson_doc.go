package util

import (
	"strconv"
	"unsafe"

	"github.com/MoeForge/moego/ojson"
)

// ForEachDocArrayElement visits the elements of the array at path in doc, in
// order, until visit returns false. Elements are read one at a time: an array
// that earlier edits have split is never serialized as a whole, and untouched
// elements are views of the input. visit may edit the element it receives but
// must not insert or remove array elements.
func ForEachDocArrayElement(doc *ojson.Doc, path string, visit func(index int, element ojson.Result) bool) {
	if doc == nil {
		return
	}
	prefix := path + "."
	first := doc.Get(prefix + "0")
	if !first.Exists() {
		return
	}
	// Writing the first element back unchanged splits the array, so the reads
	// below index into it instead of rescanning the array from its start.
	if doc.SetRaw(prefix+"0", first.Raw) != nil {
		return
	}
	element := first
	for index := 0; element.Exists(); index++ {
		if !visit(index, element) {
			return
		}
		element = doc.Get(prefix + strconv.Itoa(index+1))
	}
}

// SetDocRawOwned sets the raw JSON text at path to value without copying it.
// Ownership of value passes to doc: the caller must not modify it afterwards.
func SetDocRawOwned(doc *ojson.Doc, path string, value []byte) error {
	return doc.SetRaw(path, unsafe.String(unsafe.SliceData(value), len(value)))
}
