#!/bin/sh
# Installs tether for macOS/Linux. No sudo, ever.
#
#   curl -fsSL https://praneethravuri.github.io/tether/install.sh | sh
#
# Env overrides:
#   TETHER_VERSION      version tag to install, e.g. v0.3.2 (default: latest)
#   TETHER_INSTALL_DIR  where to put the binaries (default: ~/.local/bin)
#   TETHER_BASE_URL     override the GitHub releases base URL (testing only)
set -e

REPO="praneethravuri/tether"
BASE_URL="${TETHER_BASE_URL:-https://github.com/${REPO}/releases}"

# Wrapped in main() so a download truncated mid-transfer can't execute a
# partial script -- the function body isn't run until the closing brace,
# on the last line of the file, has been read.
main() {
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	arch="$(uname -m)"

	case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*)
		echo "tether: unsupported architecture: $arch" >&2
		exit 1
		;;
	esac

	case "$os" in
	darwin | linux) ;;
	*)
		echo "tether: unsupported OS: $os (this installer covers macOS and Linux only)" >&2
		exit 1
		;;
	esac

	if [ -n "${TETHER_VERSION:-}" ]; then
		download_url="${BASE_URL}/download/${TETHER_VERSION}"
	else
		download_url="${BASE_URL}/latest/download"
	fi

	asset="tether_${os}_${arch}.tar.gz"
	install_dir="${TETHER_INSTALL_DIR:-$HOME/.local/bin}"

	tmp_dir="$(mktemp -d)"
	trap 'rm -rf "$tmp_dir"' EXIT

	echo "tether: downloading ${asset}..."
	fetch "${download_url}/${asset}" "${tmp_dir}/${asset}"
	fetch "${download_url}/checksums.txt" "${tmp_dir}/checksums.txt"

	verify_checksum "${tmp_dir}/${asset}" "${tmp_dir}/checksums.txt" "${asset}"

	tar xzf "${tmp_dir}/${asset}" -C "${tmp_dir}"

	mkdir -p "${install_dir}"
	if [ ! -w "${install_dir}" ]; then
		echo "tether: ${install_dir} is not writable, and this installer never uses sudo." >&2
		echo "        set TETHER_INSTALL_DIR to a directory you own." >&2
		exit 1
	fi

	mv "${tmp_dir}/tether" "${install_dir}/tether"
	chmod 0755 "${install_dir}/tether"
	# Not notarized, so macOS quarantines it -- left in place on purpose,
	# since it's the one control that would catch a tampered release.
	if [ "$os" = "darwin" ]; then
		echo
		echo "tether: this binary isn't notarized, so macOS will prompt on first launch."
		echo "        Already verified the checksum above? Skip the prompt with:"
		echo "          xattr -d com.apple.quarantine ${install_dir}/tether"
	fi

	echo "tether: installed $("${install_dir}/tether" version) to ${install_dir}"

	case ":$PATH:" in
	*":${install_dir}:"*) ;;
	*)
		echo
		echo "${install_dir} is not on your PATH. Add this to your shell rc file:"
		echo "  export PATH=\"${install_dir}:\$PATH\""
		;;
	esac

	echo
	echo "Next: tether register <name>"
}

# fetch <url> <output path>
fetch() {
	if command -v curl >/dev/null 2>&1; then
		# --proto '=https' governs the initial URL only; --proto-redir covers redirects too.
		curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q --https-only "$1" -O "$2"
	else
		echo "tether: need curl or wget to install" >&2
		exit 1
	fi
}

# verify_checksum <file> <checksums file> <asset name>
verify_checksum() {
	file="$1"
	checksums="$2"
	name="$3"

	expected="$(awk -v n="$name" '$2 == n { print $1 }' "$checksums")"
	if [ -z "$expected" ]; then
		echo "tether: no checksum entry for ${name}" >&2
		exit 1
	fi

	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$file" | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$file" | awk '{print $1}')"
	else
		echo "tether: need sha256sum or shasum to verify the download" >&2
		exit 1
	fi

	if [ "$expected" != "$actual" ]; then
		rm -f "$file"
		echo "tether: checksum mismatch for ${name} -- download deleted" >&2
		echo "        expected ${expected}, got ${actual}" >&2
		exit 1
	fi
}

main
