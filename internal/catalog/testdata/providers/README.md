# Captured provider payloads

Every file here was captured verbatim from the live provider endpoint named in
its filename. They are not hand-written.

That distinction is the entire point. Every provider defect this project has
shipped had the same shape: a struct decoded a field name the upstream never
sends, and the fixture beside it was hand-written with the same wrong name — so
the test proved the struct matched itself rather than the API, and the feature
silently returned nothing. Compilation, `go vet`, the linters, and the race
detector are all blind to it.

Three checks use these files, and each catches something the others cannot:

| Check | Catches |
| --- | --- |
| `provider_contract_test.go` | a wrong tag that displaces a working one — the feature returns empty |
| `TestEveryDeclaredJSONTagAppearsInACapturedPayload` | a tag that appears in no captured payload at all |
| `TestResponseStructsMatchTheirOwnEndpointsPayload` | a tag valid on another endpoint but dead on this one |

The third exists because the second is not enough: `genres` is real on the
MusicBrainz artist *lookup* and absent from the artist *search*, so a
union-of-all-payloads check reports it as covered while the search silently
decodes nothing.

## Refreshing a capture

Re-request the endpoint and overwrite the file. Do not edit one by hand — a
hand-edited fixture is exactly the failure mode these exist to prevent. Keep
`limit` small so the files stay reviewable.

```
UA='ArtistTrackarr/contract-capture (you@example.com)'
MB=a74b1b7f-71a5-4011-9441-d0b5e4122711

curl -H "User-Agent: $UA" "https://musicbrainz.org/ws/2/artist?fmt=json&limit=3&query=Radiohead"
curl -H "User-Agent: $UA" "https://musicbrainz.org/ws/2/artist/$MB?fmt=json&inc=aliases+genres"
curl -H "User-Agent: $UA" "https://musicbrainz.org/ws/2/release-group?fmt=json&artist=$MB&type=album%7Cep%7Csingle&release-group-status=website-default&limit=3"
curl -H "User-Agent: $UA" "https://musicbrainz.org/ws/2/recording?fmt=json&limit=5&query=arid%3A$MB"
curl "https://itunes.apple.com/lookup?id=909253&entity=album&limit=3&country=US"
curl "https://itunes.apple.com/search?term=Radiohead&country=US&media=music&entity=song&attribute=artistTerm&limit=3"
curl -X POST -H 'Content-Type: application/json' \
  -d '{"artist_mbids":["'$MB'"]}' https://api.listenbrainz.org/1/popularity/artist
```

MusicBrainz rate-limits to roughly one request a second and returns
`{"error": "...busy..."}` under load; retry rather than checking in the error.

## Deliberate gaps

Some captures exist to pin behaviour that only shows up in an unusual response:

- `musicbrainz_recording_search.json` — recordings with **no** dates, which the
  credit projection must skip rather than store undated.
- `musicbrainz_recording_search_dated.json` — mixed date precision (`2012-02-27`,
  `1993`) and null release dates in one payload.
- `musicbrainz_recording_search_credits.json` — genuine multi-artist credits,
  without which the guest-credit path is never exercised.
- `itunes_compilation_song_search.json` — the only shape that carries
  `collectionArtistName`, which Apple sends on track rows but never on the
  collection rows the album lookup keeps.
- `musicbrainz_error.json` — the error envelope.

**Spotify is not captured.** Its API requires OAuth client credentials, which
this repository deliberately does not carry, so its tags are listed in
`spotifyUnverifiedTags` with a reason. Adding an entry there is a deliberate
statement that a tag is unverified against anything real; the list can only
shrink, because a test fails if a captured payload ever covers one.
