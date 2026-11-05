# Astronaut face fixtures

`sources.tsv` records Wikimedia Commons source URLs, original sizes, SHA-256 checksums, and attribution pages. All photographs depict astronauts and were published as public-domain NASA or government photographs; consult each source page for its credit and terms. The Aldrin entry uses Commons' 960-pixel preview of the same source photograph. Image files are downloaded explicitly by `.github/scripts/stage-embed-test.go` into `KIT_EMBED_TEST_DIR/faces`, outside the repository.

`boxes.json` records normalized boxes in the original image's top-left coordinate system. The Gemini-8 photograph includes Neil Armstrong in the foreground and David Scott behind him; both have separate annotations. The Scott annotation is marked `extra` so native acceptance reports the original manifest and the expanded corpus separately. It checks every annotated face and all returned same-person and different-person pairs.
