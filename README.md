# relay

Public release distribution for the `relay` CLI — unified gateway access to integrated services (Google Workspace, Clockify, Discord, invoicing, and more).

Source lives in a private repository; this repo hosts release binaries only.

## Install

### macOS (Homebrew)

```bash
brew install patrikmichi/tap/relay
```

### Windows (Scoop)

```powershell
scoop bucket add patrikmichi https://github.com/patrikmichi/scoop-bucket
scoop install relay
```

### Manual

Download the binary for your platform from [Releases](https://github.com/patrikmichi/relay/releases), verify against `SHA256SUMS.txt`, and place it on your `PATH`.

## Usage

```bash
relay login        # OAuth via Google; tokens stored in the OS keychain
relay services     # list available services
relay <service> <tool> --param=value
```
