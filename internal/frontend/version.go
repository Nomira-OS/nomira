package frontend

// Version of the frontend: the release version (semver, without "v") for
// releases, sha-<short commit> for development builds, "dev" when built
// by hand. Set at build time:
//
//	go build -ldflags "-X github.com/Nomira-OS/nomira/internal/frontend.Version=0.4.0"
//
// The nomira CLI compares it with its own version (see its README).
var Version = "dev"
