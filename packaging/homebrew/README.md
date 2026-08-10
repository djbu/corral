# Private Homebrew tap staging area

This directory has the layout expected by a Homebrew tap, but it is not a
published tap. Generate a release-specific formula only after all four release
archives and their checksums exist:

```sh
scripts/release/render-homebrew-formula.sh \
  0.7.0-rc.1 dist/corral_0.7.0-rc.1_checksums.txt \
  /path/to/private-tap/Formula/corral.rb
```

Consumers of the private formula must export a read-only token as
`HOMEBREW_GITHUB_API_TOKEN`. Never commit the generated formula to this
repository: its version and hashes belong to the corresponding release or to a
separate private tap. Public tap publication remains outside M7.
