# Builds carryout from the release source. Used by COPR (through Packit,
# see .packit.yaml) and by the release workflow's .rpm job.
#
# carryout has no module dependencies, so the build is offline unless the
# builder's Go is older than go.mod's; then the go command fetches that
# toolchain from proxy.golang.org, which needs network access in COPR
# (enable_net).

# Static, stripped Go binary: there is no debug information to package.
%global debug_package %{nil}
# The version stamped into the binary: the release version without "v".
# The release workflow passes it for pre-releases, whose RPM version uses "~".
%{!?carryout_version:%global carryout_version %{version}}

Name:           carryout
# Packit replaces Version with the release tag minus its "v" (see .packit.yaml).
# v0.3.0 is the first tag that carries build-aux/, so nothing older builds.
Version:        0.3.0
Release:        1%{?dist}
Summary:        Download helper for Google Takeout

License:        GPL-3.0-or-later
URL:            https://github.com/jm2/carryout
Source0:        https://github.com/jm2/carryout/archive/refs/tags/v%{version}.tar.gz

ExclusiveArch:  x86_64 aarch64

BuildRequires:  golang >= 1.24
BuildRequires:  make

%description
carryout fetches every archive part of a Google Takeout export, given one
download request copied from the browser's developer tools and the file
list from the Takeout page. It verifies each archive as it streams, pauses
for fresh cookies when the session dies instead of wasting download
attempts, and resumes interrupted parts.

%prep
%autosetup -p1 -n %{name}-%{version}

%build
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
export GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.org}"
export GOFLAGS="${GOFLAGS:-} -mod=readonly"
make build VERSION=%{carryout_version}

%install
mkdir -p bin-stage
cp carryout bin-stage/
build-aux/linux/install-tree.sh %{buildroot} bin-stage %{_prefix}

%check
./carryout version | grep -Fx "carryout %{carryout_version}"

%files
%license LICENSE
%doc %{_docdir}/%{name}/README.md
%{_bindir}/carryout

%changelog
* Sat Nov 07 2026 carryout contributors - 0.3.0-1
- Initial package; v0.3.0 is the first release whose source has build-aux/
