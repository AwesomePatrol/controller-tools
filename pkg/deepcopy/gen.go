/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deepcopy

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"io"
	"sort"
	"strings"

	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

// NB(directxman12): markers.LoadRoots ignores autogenerated code via a build tag
// so any time we check for existing deepcopy functions, we only seen manually written ones.

const (
	runtimeObjPath = "k8s.io/apimachinery/pkg/runtime.Object"
)

var (
	enablePkgMarker  = markers.Must(markers.MakeDefinition("kubebuilder:object:generate", markers.DescribesPackage, false))
	enableTypeMarker = markers.Must(markers.MakeDefinition("kubebuilder:object:generate", markers.DescribesType, false))
	isObjectMarker   = markers.Must(markers.MakeDefinition("kubebuilder:object:root", markers.DescribesType, false))

	legacyEnablePkgMarker  = markers.Must(markers.MakeDefinition("k8s:deepcopy-gen", markers.DescribesPackage, markers.RawArguments(nil)))
	legacyEnableTypeMarker = markers.Must(markers.MakeDefinition("k8s:deepcopy-gen", markers.DescribesType, markers.RawArguments(nil)))
	legacyIsObjectMarker   = markers.Must(markers.MakeDefinition("k8s:deepcopy-gen:interfaces", markers.DescribesType, ""))
)

// +controllertools:marker:generateHelp

// Generator generates code containing DeepCopy, DeepCopyInto, and
// DeepCopyObject method implementations.
type Generator struct {
	// HeaderFile specifies the header text (e.g. license) to prepend to generated files.
	HeaderFile string `marker:",optional"`
	// Year specifies the year to substitute for " YEAR" in the header file.
	Year string `marker:",optional"`
}

func (Generator) RegisterMarkers(into *markers.Registry) error {
	if err := markers.RegisterAll(into,
		enablePkgMarker, legacyEnablePkgMarker, enableTypeMarker,
		legacyEnableTypeMarker, isObjectMarker, legacyIsObjectMarker); err != nil {
		return err
	}
	into.AddHelp(enablePkgMarker,
		markers.SimpleHelp("object", "enables or disables object interface & deepcopy implementation generation for this package"))
	into.AddHelp(
		enableTypeMarker, markers.SimpleHelp("object", "overrides enabling or disabling deepcopy generation for this type"))
	into.AddHelp(isObjectMarker,
		markers.SimpleHelp("object", "enables object interface implementation generation for this type"))

	into.AddHelp(legacyEnablePkgMarker,
		markers.DeprecatedHelp(enablePkgMarker.Name, "object", "enables or disables object interface & deepcopy implementation generation for this package"))
	into.AddHelp(legacyEnableTypeMarker,
		markers.DeprecatedHelp(enableTypeMarker.Name, "object", "overrides enabling or disabling deepcopy generation for this type"))
	into.AddHelp(legacyIsObjectMarker,
		markers.DeprecatedHelp(isObjectMarker.Name, "object", "enables object interface implementation generation for this type"))
	return nil
}

func enabledOnPackage(col *markers.Collector, pkg *loader.Package) (bool, error) {
	pkgMarkers, err := markers.PackageMarkers(col, pkg)
	if err != nil {
		return false, err
	}
	pkgMarker := pkgMarkers.Get(enablePkgMarker.Name)
	if pkgMarker != nil {
		return pkgMarker.(bool), nil
	}
	legacyMarker := pkgMarkers.Get(legacyEnablePkgMarker.Name)
	if legacyMarker != nil {
		legacyMarkerVal := string(legacyMarker.(markers.RawArguments))
		firstArg := strings.Split(legacyMarkerVal, ",")[0]
		return firstArg == "package", nil
	}

	return false, nil
}

func enabledOnType(allTypes bool, info *markers.TypeInfo) bool {
	if typeMarker := info.Markers.Get(enableTypeMarker.Name); typeMarker != nil {
		return typeMarker.(bool)
	}
	legacyMarker := info.Markers.Get(legacyEnableTypeMarker.Name)
	if legacyMarker != nil {
		legacyMarkerVal := string(legacyMarker.(markers.RawArguments))
		return legacyMarkerVal == "true"
	}
	return allTypes || genObjectInterface(info)
}

func genObjectInterface(info *markers.TypeInfo) bool {
	objectEnabled := info.Markers.Get(isObjectMarker.Name)
	if objectEnabled != nil {
		return objectEnabled.(bool)
	}

	for _, legacyEnabled := range info.Markers[legacyIsObjectMarker.Name] {
		if legacyEnabled == runtimeObjPath {
			return true
		}
	}
	return false
}

func (d Generator) Generate(ctx *genall.GenerationContext) error {
	var headerText string

	if d.HeaderFile != "" {
		headerBytes, err := ctx.ReadFile(d.HeaderFile)
		if err != nil {
			return err
		}
		headerText = string(headerBytes)
	}
	headerText = strings.ReplaceAll(headerText, " YEAR", " "+d.Year)

	objGenCtx := ObjectGenCtx{
		Collector:  ctx.Collector,
		Checker:    ctx.Checker,
		HeaderText: headerText,
	}

	for _, root := range ctx.Roots {
		outContents := objGenCtx.GenerateForPackage(root)
		if outContents == nil {
			continue
		}

		writeOut(ctx, root, outContents)
	}

	return nil
}

// ObjectGenCtx contains the common info for generating deepcopy implementations.
// It mostly exists so that generating for a package can be easily tested without
// requiring a full set of output rules, etc.
type ObjectGenCtx struct {
	Collector  *markers.Collector
	Checker    *loader.TypeChecker
	HeaderText string
}

// writeHeader writes out the build tag, package declaration, and imports
func writeHeader(pkg *loader.Package, out io.Writer, packageName string, imports *importsList, headerText string) {
	// NB(directxman12): blank line after build tags to distinguish them from comments
	_, err := fmt.Fprintf(out, `//go:build !ignore_autogenerated
// +build !ignore_autogenerated

%[3]s

// Code generated by controller-gen. DO NOT EDIT.

package %[1]s

import (
%[2]s
)

`, packageName, strings.Join(imports.ImportSpecs(), "\n"), headerText)
	if err != nil {
		pkg.AddError(err)
	}

}

// GenerateForPackage generates DeepCopy and runtime.Object implementations for
// types in the given package, writing the formatted result to given writer.
// May return nil if source could not be generated.
func (ctx *ObjectGenCtx) GenerateForPackage(root *loader.Package) []byte {
	allTypes, err := enabledOnPackage(ctx.Collector, root)
	if err != nil {
		root.AddError(err)
		return nil
	}

	ctx.Checker.Check(root, func(node ast.Node) bool {
		// ignore interfaces
		_, isIface := node.(*ast.InterfaceType)
		return !isIface
	})

	root.NeedTypesInfo()

	byType := make(map[string][]byte)
	imports := &importsList{
		byPath:  make(map[string]string),
		byAlias: make(map[string]string),
		pkg:     root,
	}
	// avoid confusing aliases by "reserving" the root package's name as an alias
	imports.byAlias[root.Name] = ""

	if err := markers.EachType(ctx.Collector, root, func(info *markers.TypeInfo) {
		outContent := new(bytes.Buffer)

		// copy when nabled for all types and not disabled, or enabled
		// specifically on this type
		if !enabledOnType(allTypes, info) {
			return
		}

		// avoid copying non-exported types, etc
		if !shouldBeCopied(root, info) {
			return
		}

		copyCtx := &copyMethodMaker{
			pkg:         root,
			importsList: imports,
			codeWriter:  &codeWriter{out: outContent},
		}

		copyCtx.GenerateMethodsFor(root, info)

		outBytes := outContent.Bytes()
		if len(outBytes) > 0 {
			byType[info.Name] = outBytes
		}
	}); err != nil {
		root.AddError(err)
		return nil
	}

	if len(byType) == 0 {
		return nil
	}

	outContent := new(bytes.Buffer)
	writeHeader(root, outContent, root.Name, imports, ctx.HeaderText)
	writeMethods(root, outContent, byType)

	outBytes := outContent.Bytes()
	formattedBytes, err := format.Source(outBytes)
	if err != nil {
		root.AddError(err)
		// we still write the invalid source to disk to figure out what went wrong
	} else {
		outBytes = formattedBytes
	}

	return outBytes
}

// writeMethods writes each method to the file, sorted by type name.
func writeMethods(pkg *loader.Package, out io.Writer, byType map[string][]byte) {
	sortedNames := make([]string, 0, len(byType))
	for name := range byType {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	for _, name := range sortedNames {
		_, err := out.Write(byType[name])
		if err != nil {
			pkg.AddError(err)
		}
	}
}

// writeFormatted outputs the given code, after gofmt-ing it.  If we couldn't gofmt,
// we write the unformatted code for debugging purposes.
func writeOut(ctx *genall.GenerationContext, root *loader.Package, outBytes []byte) {
	outputFile, err := ctx.Open(root, "zz_generated.deepcopy.go")
	if err != nil {
		root.AddError(err)
		return
	}
	defer outputFile.Close()
	n, err := outputFile.Write(outBytes)
	if err != nil {
		root.AddError(err)
		return
	}
	if n < len(outBytes) {
		root.AddError(io.ErrShortWrite)
	}
}
