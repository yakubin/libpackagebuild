/*******************************************************************************
*
* Copyright 2015-2018 Stefan Majewsky <majewsky@gmx.net>
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You should have received a copy of the License along with this
* program. If not, you may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
*******************************************************************************/

//Package pacman provides a build.Generator for Pacman packages (as used by Arch Linux).
package pacman

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	build "github.com/holocm/libpackagebuild"
	"github.com/holocm/libpackagebuild/filesystem"
)

//Generator is the build.Generator for Pacman packages (as used by Arch Linux
//and derivatives).
type Generator struct {
	Package *build.Package
}

//GeneratorFactory spawns Generator instances. It satisfies the build.GeneratorFactory type.
func GeneratorFactory(pkg *build.Package) build.Generator {
	return &Generator{Package: pkg}
}

var archMap = map[build.Architecture]string{
	build.ArchitectureAny:     "any",
	build.ArchitectureI386:    "i686",
	build.ArchitectureX86_64:  "x86_64",
	build.ArchitectureARMv5:   "arm",
	build.ArchitectureARMv6h:  "armv6h",
	build.ArchitectureARMv7h:  "armv7h",
	build.ArchitectureAArch64: "aarch64",
}

//RecommendedFileName implements the build.Generator interface.
func (g *Generator) RecommendedFileName() string {
	//this is called after Build(), so we can assume that package name,
	//version, etc. were already validated
	pkg := g.Package
	return fmt.Sprintf("%s-%s-%s.pkg.tar.xz", pkg.Name, fullVersionString(pkg), archMap[pkg.Architecture])
}

//Validate implements the build.Generator interface.
func (g *Generator) Validate() []error {
	var nameRx = `[a-z0-9@._+][a-z0-9@._+-]*`
	var versionRx = `[a-zA-Z0-9._]+`
	return g.Package.ValidateWith(build.RegexSet{
		PackageName:    nameRx,
		PackageVersion: versionRx,
		RelatedName:    "(?:except:)?(?:group:)?" + nameRx,
		RelatedVersion: "(?:[0-9]+:)?" + versionRx + "(?:-[1-9][0-9]*)?", //incl. release/epoch
		FormatName:     "pacman",
	}, archMap)
}

//Build implements the build.Generator interface.
func (g *Generator) Build() ([]byte, error) {
	pkg := g.Package
	pkg.PrepareBuild()

	//write .PKGINFO
	err := writePKGINFO(pkg)
	if err != nil {
		return nil, fmt.Errorf("Failed to write .PKGINFO: %s", err.Error())
	}

	//write .INSTALL
	writeINSTALL(pkg)

	//write mtree
	err = writeMTREE(pkg)
	if err != nil {
		return nil, fmt.Errorf("Failed to write .MTREE: %s", err.Error())
	}

	//compress package
	var buf bytes.Buffer
	err = pkg.FSRoot.ToTarXZArchive(&buf, false, true)
	return buf.Bytes(), err
}

func fullVersionString(pkg *build.Package) string {
	str := fmt.Sprintf("%s-%d", pkg.Version, pkg.Release)
	if pkg.Epoch > 0 {
		str = fmt.Sprintf("%d:%s", pkg.Epoch, str)
	}
	return str
}

func writePKGINFO(pkg *build.Package) error {
	//normalize package description like makepkg does
	desc := regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(pkg.Description), " ")

	//generate .PKGINFO
	contents := "# Generated by holo-build\n"
	contents += fmt.Sprintf("pkgname = %s\n", pkg.Name)
	contents += fmt.Sprintf("pkgver = %s\n", fullVersionString(pkg))
	contents += fmt.Sprintf("pkgdesc = %s\n", desc)
	contents += "url = \n"
	if pkg.Author == "" {
		contents += "packager = Unknown Packager\n"
	} else {
		contents += fmt.Sprintf("packager = %s\n", pkg.Author)
	}
	contents += fmt.Sprintf("size = %d\n", pkg.FSRoot.InstalledSizeInBytes())
	contents += fmt.Sprintf("arch = %s\n", archMap[pkg.Architecture])
	contents += "license = custom:none\n"
	replaces, err := compilePackageRequirements("replaces", pkg.Replaces)
	if err != nil {
		return err
	}
	conflicts, err := compilePackageRequirements("conflict", pkg.Conflicts)
	if err != nil {
		return err
	}
	provides, err := compilePackageRequirements("provides", pkg.Provides)
	if err != nil {
		return err
	}
	contents += replaces + conflicts + provides
	contents += compileBackupMarkers(pkg)
	requires, err := compilePackageRequirements("depend", pkg.Requires)
	if err != nil {
		return err
	}
	contents += requires

	//we used holo-build to build this, so the build depends on this package
	contents += "makedepend = holo-build\n"
	//these makepkgopt are fabricated (well, duh) and describe the behavior of
	//holo-build in terms of these options
	contents += "makepkgopt = !strip\n"
	contents += "makepkgopt = docs\n"
	contents += "makepkgopt = libtool\n"
	contents += "makepkgopt = staticlibs\n"
	contents += "makepkgopt = emptydirs\n"
	contents += "makepkgopt = !zipman\n"
	contents += "makepkgopt = !purge\n"
	contents += "makepkgopt = !upx\n"
	contents += "makepkgopt = !debug\n"

	//write .PKGINFO
	pkg.FSRoot.Entries[".PKGINFO"] = &filesystem.RegularFile{
		Content:  contents,
		Metadata: filesystem.NodeMetadata{Mode: 0644},
	}
	return nil
}

func compileBackupMarkers(pkg *build.Package) string {
	var lines []string
	pkg.WalkFSWithRelativePaths(func(path string, node filesystem.Node) error {
		if _, ok := node.(*filesystem.RegularFile); !ok {
			return nil //look only at regular files
		}
		if !strings.HasPrefix(path, "usr/share/holo/") {
			lines = append(lines, fmt.Sprintf("backup = %s\n", path))
		}
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "")
}

func writeINSTALL(pkg *build.Package) {
	//assemble the contents for the .INSTALL file
	contents := ""
	if script := pkg.Script(build.SetupAction); script != "" {
		contents += fmt.Sprintf("post_install() {\n%s\n}\npost_upgrade() {\npost_install\n}\n", script)
	}
	if script := pkg.Script(build.CleanupAction); script != "" {
		contents += fmt.Sprintf("post_remove() {\n%s\n}\n", script)
	}

	//do we need the .INSTALL file at all?
	if contents == "" {
		return
	}

	pkg.FSRoot.Entries[".INSTALL"] = &filesystem.RegularFile{
		Content:  contents,
		Metadata: filesystem.NodeMetadata{Mode: 0644},
	}
}

func writeMTREE(pkg *build.Package) error {
	contents, err := makeMTREE(pkg)
	if err != nil {
		return err
	}

	pkg.FSRoot.Entries[".MTREE"] = &filesystem.RegularFile{
		Content:  string(contents),
		Metadata: filesystem.NodeMetadata{Mode: 0644},
	}
	return nil
}
