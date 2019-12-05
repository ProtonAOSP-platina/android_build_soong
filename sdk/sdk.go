// Copyright (C) 2019 The Android Open Source Project
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sdk

import (
	"fmt"
	"io"
	"strconv"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
	// This package doesn't depend on the apex package, but import it to make its mutators to be
	// registered before mutators in this package. See RegisterPostDepsMutators for more details.
	_ "android/soong/apex"
	"android/soong/cc"
	"android/soong/java"
)

func init() {
	pctx.Import("android/soong/android")
	pctx.Import("android/soong/java/config")

	android.RegisterModuleType("sdk", ModuleFactory)
	android.RegisterModuleType("sdk_snapshot", SnapshotModuleFactory)
	android.PreDepsMutators(RegisterPreDepsMutators)
	android.PostDepsMutators(RegisterPostDepsMutators)

	// Populate the dependency tags for each member list property.  This needs to
	// be done here to break an initialization cycle.
	for _, memberListProperty := range sdkMemberListProperties {
		memberListProperty.dependencyTag = &sdkMemberDependencyTag{
			memberListProperty: memberListProperty,
		}
	}
}

type sdk struct {
	android.ModuleBase
	android.DefaultableModuleBase

	properties sdkProperties

	snapshotFile android.OptionalPath

	// The builder, preserved for testing.
	builderForTests *snapshotBuilder
}

type sdkProperties struct {
	// The list of java header libraries in this SDK
	//
	// This should be used for java libraries that are provided separately at runtime,
	// e.g. through an APEX.
	Java_header_libs []string
	// The list of java implementation libraries in this SDK
	Java_libs []string
	// The list of native libraries in this SDK
	Native_shared_libs []string
	// The list of stub sources in this SDK
	Stubs_sources []string

	Snapshot bool `blueprint:"mutated"`
}

type sdkMemberDependencyTag struct {
	blueprint.BaseDependencyTag
	memberListProperty *sdkMemberListProperty
}

// Contains information about the sdk properties that list sdk members, e.g.
// Java_header_libs.
type sdkMemberListProperty struct {
	// the name of the property as used in a .bp file
	name string

	// getter for the list of member names
	getter func(properties *sdkProperties) []string

	// the type of member referenced in the list
	memberType android.SdkMemberType

	// the dependency tag used for items in this list.
	dependencyTag *sdkMemberDependencyTag
}

// Information about how to handle each member list property.
//
// It is organized first by package and then by name within the package.
// Packages are in alphabetical order and properties are in alphabetical order
// within each package.
var sdkMemberListProperties = []*sdkMemberListProperty{
	// Members from cc package.
	{
		name:       "native_shared_libs",
		getter:     func(properties *sdkProperties) []string { return properties.Native_shared_libs },
		memberType: cc.LibrarySdkMemberType,
	},
	// Members from java package.
	{
		name:       "java_header_libs",
		getter:     func(properties *sdkProperties) []string { return properties.Java_header_libs },
		memberType: java.HeaderLibrarySdkMemberType,
	},
	{
		name:       "java_libs",
		getter:     func(properties *sdkProperties) []string { return properties.Java_libs },
		memberType: java.ImplLibrarySdkMemberType,
	},
	{
		name:       "stubs_sources",
		getter:     func(properties *sdkProperties) []string { return properties.Stubs_sources },
		memberType: java.DroidStubsSdkMemberType,
	},
}

// sdk defines an SDK which is a logical group of modules (e.g. native libs, headers, java libs, etc.)
// which Mainline modules like APEX can choose to build with.
func ModuleFactory() android.Module {
	s := &sdk{}
	s.AddProperties(&s.properties)
	android.InitAndroidMultiTargetsArchModule(s, android.HostAndDeviceSupported, android.MultilibCommon)
	android.InitDefaultableModule(s)
	android.AddLoadHook(s, func(ctx android.LoadHookContext) {
		type props struct {
			Compile_multilib *string
		}
		p := &props{Compile_multilib: proptools.StringPtr("both")}
		ctx.AppendProperties(p)
	})
	return s
}

// sdk_snapshot is a versioned snapshot of an SDK. This is an auto-generated module.
func SnapshotModuleFactory() android.Module {
	s := ModuleFactory()
	s.(*sdk).properties.Snapshot = true
	return s
}

func (s *sdk) snapshot() bool {
	return s.properties.Snapshot
}

func (s *sdk) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	if !s.snapshot() {
		// We don't need to create a snapshot out of sdk_snapshot.
		// That doesn't make sense. We need a snapshot to create sdk_snapshot.
		s.snapshotFile = android.OptionalPathForPath(s.buildSnapshot(ctx))
	}
}

func (s *sdk) AndroidMkEntries() android.AndroidMkEntries {
	if !s.snapshotFile.Valid() {
		return android.AndroidMkEntries{}
	}

	return android.AndroidMkEntries{
		Class:      "FAKE",
		OutputFile: s.snapshotFile,
		DistFile:   s.snapshotFile,
		Include:    "$(BUILD_PHONY_PACKAGE)",
		ExtraFooters: []android.AndroidMkExtraFootersFunc{
			func(w io.Writer, name, prefix, moduleDir string, entries *android.AndroidMkEntries) {
				// Allow the sdk to be built by simply passing its name on the command line.
				fmt.Fprintln(w, ".PHONY:", s.Name())
				fmt.Fprintln(w, s.Name()+":", s.snapshotFile.String())
			},
		},
	}
}

// RegisterPreDepsMutators registers pre-deps mutators to support modules implementing SdkAware
// interface and the sdk module type. This function has been made public to be called by tests
// outside of the sdk package
func RegisterPreDepsMutators(ctx android.RegisterMutatorsContext) {
	ctx.BottomUp("SdkMember", memberMutator).Parallel()
	ctx.TopDown("SdkMember_deps", memberDepsMutator).Parallel()
	ctx.BottomUp("SdkMemberInterVersion", memberInterVersionMutator).Parallel()
}

// RegisterPostDepshMutators registers post-deps mutators to support modules implementing SdkAware
// interface and the sdk module type. This function has been made public to be called by tests
// outside of the sdk package
func RegisterPostDepsMutators(ctx android.RegisterMutatorsContext) {
	// These must run AFTER apexMutator. Note that the apex package is imported even though there is
	// no direct dependency to the package here. sdkDepsMutator sets the SDK requirements from an
	// APEX to its dependents. Since different versions of the same SDK can be used by different
	// APEXes, the apex and its dependents (which includes the dependencies to the sdk members)
	// should have been mutated for the apex before the SDK requirements are set.
	ctx.TopDown("SdkDepsMutator", sdkDepsMutator).Parallel()
	ctx.BottomUp("SdkDepsReplaceMutator", sdkDepsReplaceMutator).Parallel()
	ctx.TopDown("SdkRequirementCheck", sdkRequirementsMutator).Parallel()
}

type dependencyTag struct {
	blueprint.BaseDependencyTag
}

// For dependencies from an in-development version of an SDK member to frozen versions of the same member
// e.g. libfoo -> libfoo.mysdk.11 and libfoo.mysdk.12
type sdkMemberVesionedDepTag struct {
	dependencyTag
	member  string
	version string
}

// Step 1: create dependencies from an SDK module to its members.
func memberMutator(mctx android.BottomUpMutatorContext) {
	if m, ok := mctx.Module().(*sdk); ok {
		for _, memberListProperty := range sdkMemberListProperties {
			names := memberListProperty.getter(&m.properties)
			tag := memberListProperty.dependencyTag
			memberListProperty.memberType.AddDependencies(mctx, tag, names)
		}
	}
}

// Step 2: record that dependencies of SDK modules are members of the SDK modules
func memberDepsMutator(mctx android.TopDownMutatorContext) {
	if s, ok := mctx.Module().(*sdk); ok {
		mySdkRef := android.ParseSdkRef(mctx, mctx.ModuleName(), "name")
		if s.snapshot() && mySdkRef.Unversioned() {
			mctx.PropertyErrorf("name", "sdk_snapshot should be named as <name>@<version>. "+
				"Did you manually modify Android.bp?")
		}
		if !s.snapshot() && !mySdkRef.Unversioned() {
			mctx.PropertyErrorf("name", "sdk shouldn't be named as <name>@<version>.")
		}
		if mySdkRef.Version != "" && mySdkRef.Version != "current" {
			if _, err := strconv.Atoi(mySdkRef.Version); err != nil {
				mctx.PropertyErrorf("name", "version %q is neither a number nor \"current\"", mySdkRef.Version)
			}
		}

		mctx.VisitDirectDeps(func(child android.Module) {
			if member, ok := child.(android.SdkAware); ok {
				member.MakeMemberOf(mySdkRef)
			}
		})
	}
}

// Step 3: create dependencies from the unversioned SDK member to snapshot versions
// of the same member. By having these dependencies, they are mutated for multiple Mainline modules
// (apex and apk), each of which might want different sdks to be built with. For example, if both
// apex A and B are referencing libfoo which is a member of sdk 'mysdk', the two APEXes can be
// built with libfoo.mysdk.11 and libfoo.mysdk.12, respectively depending on which sdk they are
// using.
func memberInterVersionMutator(mctx android.BottomUpMutatorContext) {
	if m, ok := mctx.Module().(android.SdkAware); ok && m.IsInAnySdk() {
		if !m.ContainingSdk().Unversioned() {
			memberName := m.MemberName()
			tag := sdkMemberVesionedDepTag{member: memberName, version: m.ContainingSdk().Version}
			mctx.AddReverseDependency(mctx.Module(), tag, memberName)
		}
	}
}

// Step 4: transitively ripple down the SDK requirements from the root modules like APEX to its
// descendants
func sdkDepsMutator(mctx android.TopDownMutatorContext) {
	if m, ok := mctx.Module().(android.SdkAware); ok {
		// Module types for Mainline modules (e.g. APEX) are expected to implement RequiredSdks()
		// by reading its own properties like `uses_sdks`.
		requiredSdks := m.RequiredSdks()
		if len(requiredSdks) > 0 {
			mctx.VisitDirectDeps(func(m android.Module) {
				if dep, ok := m.(android.SdkAware); ok {
					dep.BuildWithSdks(requiredSdks)
				}
			})
		}
	}
}

// Step 5: if libfoo.mysdk.11 is in the context where version 11 of mysdk is requested, the
// versioned module is used instead of the un-versioned (in-development) module libfoo
func sdkDepsReplaceMutator(mctx android.BottomUpMutatorContext) {
	if m, ok := mctx.Module().(android.SdkAware); ok && m.IsInAnySdk() {
		if sdk := m.ContainingSdk(); !sdk.Unversioned() {
			if m.RequiredSdks().Contains(sdk) {
				// Note that this replacement is done only for the modules that have the same
				// variations as the current module. Since current module is already mutated for
				// apex references in other APEXes are not affected by this replacement.
				memberName := m.MemberName()
				mctx.ReplaceDependencies(memberName)
			}
		}
	}
}

// Step 6: ensure that the dependencies from outside of the APEX are all from the required SDKs
func sdkRequirementsMutator(mctx android.TopDownMutatorContext) {
	if m, ok := mctx.Module().(interface {
		DepIsInSameApex(ctx android.BaseModuleContext, dep android.Module) bool
		RequiredSdks() android.SdkRefs
	}); ok {
		requiredSdks := m.RequiredSdks()
		if len(requiredSdks) == 0 {
			return
		}
		mctx.VisitDirectDeps(func(dep android.Module) {
			if mctx.OtherModuleDependencyTag(dep) == android.DefaultsDepTag {
				// dependency to defaults is always okay
				return
			}

			// If the dep is from outside of the APEX, but is not in any of the
			// required SDKs, we know that the dep is a violation.
			if sa, ok := dep.(android.SdkAware); ok {
				if !m.DepIsInSameApex(mctx, dep) && !requiredSdks.Contains(sa.ContainingSdk()) {
					mctx.ModuleErrorf("depends on %q (in SDK %q) that isn't part of the required SDKs: %v",
						sa.Name(), sa.ContainingSdk(), requiredSdks)
				}
			}
		})
	}
}
