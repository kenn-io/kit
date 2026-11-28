# Provenance invariants

- Never substitute source time for build time.
- Keep the full commit hash in the record and check.
- Keep release-channel policy in consumers, not Kit.
- Check the selected timestamp against a nonzero caller-supplied expected instant; equal build and source times are valid.
- Require an explicit distribution expectation and a supported record schema version.
