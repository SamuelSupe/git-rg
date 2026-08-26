#!/usr/bin/env sh

set -eu

repository="SamuelSupe/git-rg"
repository_url="https://github.com/${repository}"
version_arg="latest"

die() {
    printf 'git-rg installer: %s\n' "$*" >&2
    exit 1
}

usage() {
    printf '%s\n' \
        'Usage: install.sh [--version VERSION] [--bin-dir DIRECTORY]' \
        '' \
        'Install a verified git-rg release without sudo.' \
        '' \
        'Options:' \
        '  --version VERSION  Release tag (v0.2.0 or 0.2.0); default: latest' \
        '  --bin-dir DIRECTORY Install directory; default: $HOME/.local/bin' \
        '  -h, --help         Show this help'
}

home_dir=${HOME:-}
bin_dir=''
bin_dir_explicit=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            [ "$#" -ge 2 ] || die '--version requires a value'
            version_arg=$2
            shift 2
            ;;
        --version=*)
            version_arg=${1#*=}
            shift
            ;;
        --bin-dir)
            [ "$#" -ge 2 ] || die '--bin-dir requires a value'
            bin_dir=$2
            bin_dir_explicit=1
            shift 2
            ;;
        --bin-dir=*)
            bin_dir=${1#*=}
            bin_dir_explicit=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        --)
            shift
            [ "$#" -eq 0 ] || die 'unexpected positional arguments'
            ;;
        *)
            die "unknown argument: $1 (use --help for usage)"
            ;;
    esac
done

if [ "$bin_dir_explicit" -eq 0 ]; then
    [ -n "$home_dir" ] || die 'HOME is not set; pass --bin-dir explicitly'
    bin_dir="$home_dir/.local/bin"
fi
[ -n "$bin_dir" ] || die '--bin-dir must not be empty'

case "$(uname -s)" in
    Linux)
        release_os=linux
        ;;
    Darwin)
        release_os=darwin
        ;;
    *)
        die "unsupported operating system: $(uname -s); prebuilt releases support Linux and macOS"
        ;;
esac

case "$(uname -m)" in
    x86_64|amd64)
        release_arch=amd64
        ;;
    arm64|aarch64)
        release_arch=arm64
        ;;
    *)
        die "unsupported architecture: $(uname -m); prebuilt releases support amd64 and arm64"
        ;;
esac

command -v curl >/dev/null 2>&1 || die 'curl is required'
command -v tar >/dev/null 2>&1 || die 'tar is required'
command -v mktemp >/dev/null 2>&1 || die 'mktemp is required'
if command -v sha256sum >/dev/null 2>&1; then
    hash_command=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    hash_command=shasum
else
    die 'sha256sum or shasum is required'
fi

validate_version() {
    candidate=$1
    if ! printf '%s\n' "$candidate" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$'; then
        die "invalid release version: $candidate (expected v0.2.0 or 0.2.0)"
    fi
}

if [ "$version_arg" = latest ]; then
    latest_url=$(curl -fL --silent --show-error --retry 3 --retry-delay 1 \
        --connect-timeout 15 --max-time 120 -o /dev/null -w '%{url_effective}' \
        "${repository_url}/releases/latest") || die 'could not resolve the latest GitHub release'
    case "$latest_url" in
        "${repository_url}/releases/tag/"*)
            version=${latest_url##*/}
            ;;
        *)
            die 'GitHub returned an unexpected latest-release URL'
            ;;
    esac
else
    case "$version_arg" in
        v*)
            version=$version_arg
            ;;
        *)
            version="v${version_arg}"
            ;;
    esac
fi
validate_version "$version"

asset="git-rg_${version}_${release_os}_${release_arch}.tar.gz"
archive_dir="${asset%.tar.gz}"
expected_binary="${archive_dir}/git-rg"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/git-rg.XXXXXX") || die 'could not create a temporary directory'
staged_file=''

cleanup() {
    if [ -n "$staged_file" ]; then
        rm -f "$staged_file" 2>/dev/null || :
    fi
    rm -rf "$tmp_dir" 2>/dev/null || :
}
trap cleanup EXIT HUP INT TERM

download() {
    url=$1
    destination=$2
    if ! curl -fL --silent --show-error --retry 3 --retry-delay 1 \
        --connect-timeout 15 --max-time 300 -o "$destination" "$url"; then
        die "download failed: $url"
    fi
}

base_url="${repository_url}/releases/download/${version}"
checksums_file="${tmp_dir}/checksums.txt"
archive_file="${tmp_dir}/${asset}"
download "${base_url}/checksums.txt" "$checksums_file"
download "${base_url}/${asset}" "$archive_file"

expected_checksum=$(awk -v asset="$asset" '
    BEGIN { count = 0; valid = 1 }
    $2 == asset {
        count++
        checksum = $1
        if (NF != 2 || length($1) != 64 || $1 !~ /^[0-9A-Fa-f]+$/) {
            valid = 0
        }
    }
    END {
        if (count != 1 || valid != 1) {
            exit 1
        }
        print checksum
    }
' "$checksums_file") || die "checksums.txt does not contain one valid entry for ${asset}"

if [ "$hash_command" = sha256sum ]; then
    actual_checksum=$(sha256sum "$archive_file" | awk '{print $1}') || die 'could not calculate the archive checksum'
else
    actual_checksum=$(shasum -a 256 "$archive_file" | awk '{print $1}') || die 'could not calculate the archive checksum'
fi
expected_checksum=$(printf '%s' "$expected_checksum" | tr '[:upper:]' '[:lower:]')
actual_checksum=$(printf '%s' "$actual_checksum" | tr '[:upper:]' '[:lower:]')
[ "$actual_checksum" = "$expected_checksum" ] || die "checksum verification failed for ${asset}"

listing_file="${tmp_dir}/archive.list"
tar -tvzf "$archive_file" > "$listing_file" 2>/dev/null || die "could not read verified archive ${asset}"
awk -v expected_dir="${archive_dir}/" -v expected_binary="$expected_binary" '
    {
        entry_type = substr($1, 1, 1)
        entry_name = $NF
        if (entry_name == expected_dir) {
            if (entry_type != "d") exit 1
            directory_count++
        } else if (entry_name == expected_binary) {
            if (entry_type != "-") exit 1
            binary_count++
        } else {
            exit 1
        }
    }
    END {
        if (directory_count != 1 || binary_count != 1) exit 1
    }
' "$listing_file" || die "archive ${asset} contains unexpected files or links"

extract_dir="${tmp_dir}/extracted"
mkdir "$extract_dir" || die 'could not create the extraction directory'
tar -xzf "$archive_file" -C "$extract_dir" 2>/dev/null || die "could not extract verified archive ${asset}"
binary_file="${extract_dir}/${expected_binary}"
[ -f "$binary_file" ] || die 'release archive did not contain git-rg'
if [ -L "$binary_file" ]; then
    die 'release archive contained a symlink instead of git-rg'
fi

mkdir -p "$bin_dir" || die "could not create install directory: ${bin_dir}"
target_file="${bin_dir}/git-rg"
staged_file=$(mktemp "${bin_dir}/.git-rg.XXXXXX") || die "could not create a temporary file in ${bin_dir}"
cp "$binary_file" "$staged_file" || die 'could not stage git-rg for installation'
chmod 0755 "$staged_file" || die 'could not set git-rg executable permissions'
reported_version=$("$staged_file" --version 2>/dev/null) || die 'verified archive contains a binary that cannot run on this platform'
[ "$reported_version" = "git-rg ${version}" ] || die "verified archive contains unexpected version: ${reported_version}"
mv -f "$staged_file" "$target_file" || die "could not install git-rg to ${target_file}"
staged_file=''

printf 'Installed git-rg %s to %s\n' "$version" "$target_file"
case ":${PATH:-}:" in
    *":${bin_dir}:"*)
        ;;
    *)
        printf 'Add this directory to PATH before using git-rg:\n  export PATH="%s:$PATH"\n' "$bin_dir"
        ;;
esac
