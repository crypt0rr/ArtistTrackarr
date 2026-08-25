package catalog

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This check exists because a wrong JSON tag is invisible to every other gate.
//
// The provider bugs this project has hit all had the same shape: a struct
// decoded a field name the upstream never sends, and the fixture beside it was
// hand-written with the same wrong name, so the test proved the struct matched
// itself. Compilation, vet, the linters, and the race detector are all blind to
// it, and the feature simply returns nothing.
//
// A behavioural contract test (provider_contract_test.go) catches a tag that
// displaces a working one. It cannot catch a tag that is merely dead, because a
// dead tag and an absent tag produce identical output. That is what this check
// is for: every JSON tag declared in this package must appear in at least one
// payload captured verbatim from the live provider.

// spotifyUnverifiedTags are the tags this repository cannot verify against a
// captured payload. Spotify's API requires OAuth client credentials, which are
// deliberately not carried here, so its responses cannot be recorded in CI.
//
// Adding an entry is a deliberate statement that a tag is unverified against
// anything real. That friction is the point. If a payload for one of these is
// ever captured, the check below fails until the entry is removed, so the list
// can only shrink.
var spotifyUnverifiedTags = map[string]string{
	"access_token":           "Spotify token response",
	"expires_in":             "Spotify token response",
	"message":                "Spotify error envelope",
	"reason":                 "Spotify error envelope",
	"items":                  "Spotify paged collection",
	"total":                  "Spotify paged collection",
	"external_urls":          "Spotify artist and album objects",
	"images":                 "Spotify artist and album objects",
	"width":                  "Spotify image rendition",
	"album_group":            "Spotify simplified album object",
	"album_type":             "Spotify simplified album object",
	"release_date":           "Spotify simplified album object",
	"release_date_precision": "Spotify simplified album object",
	"total_tracks":           "Spotify simplified album object",
}

// declaredJSONTags returns every JSON tag name declared by a struct field in the
// package's non-test sources, mapped to the files declaring it.
func declaredJSONTags(t *testing.T) map[string][]string {
	t.Helper()
	tags := map[string][]string{}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			structType, ok := node.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			for _, field := range structType.Fields.List {
				if field.Tag == nil {
					continue
				}
				raw, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					continue
				}
				name := strings.Split(reflect.StructTag(raw).Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				tags[name] = append(tags[name], path)
			}
			return true
		})
	}
	if len(tags) == 0 {
		t.Fatal("no JSON tags found; the check would pass vacuously")
	}
	return tags
}

// capturedPayloadKeys returns every key appearing anywhere in the captured
// provider payloads.
func capturedPayloadKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, nested := range typed {
				keys[key] = true
				walk(nested)
			}
		case []any:
			for _, nested := range typed {
				walk(nested)
			}
		}
	}
	paths, err := filepath.Glob(filepath.Join("testdata", "providers", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no captured payloads; the check would pass vacuously")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var payload any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("captured payload %s is not valid JSON: %v", filepath.Base(path), err)
		}
		walk(payload)
	}
	return keys
}

func TestEveryDeclaredJSONTagAppearsInACapturedPayload(t *testing.T) {
	tags := declaredJSONTags(t)
	keys := capturedPayloadKeys(t)
	var unverified []string
	for name, files := range tags {
		if keys[name] {
			continue
		}
		if _, allowed := spotifyUnverifiedTags[name]; allowed {
			continue
		}
		sort.Strings(files)
		unverified = append(unverified, name+" (declared in "+strings.Join(files, ", ")+")")
	}
	if len(unverified) > 0 {
		sort.Strings(unverified)
		t.Fatalf("these JSON tags appear in no captured provider payload, so nothing proves the upstream sends them:\n  %s\n\n"+
			"Either capture a real response containing the field into testdata/providers, correct the tag, or - only if the\n"+
			"provider genuinely cannot be captured here - record it in spotifyUnverifiedTags with a reason.",
			strings.Join(unverified, "\n  "))
	}
}

func TestUnverifiedTagListStaysHonest(t *testing.T) {
	tags := declaredJSONTags(t)
	keys := capturedPayloadKeys(t)
	for name, reason := range spotifyUnverifiedTags {
		if _, declared := tags[name]; !declared {
			t.Errorf("spotifyUnverifiedTags lists %q (%s) but no struct declares it; remove the stale entry", name, reason)
		}
		if keys[name] {
			t.Errorf("spotifyUnverifiedTags lists %q (%s) but a captured payload now contains it; remove the entry, it is verified", name, reason)
		}
	}
}

// endpointContracts binds a response-decoding function to the captured payloads
// its own endpoint returns.
//
// The union check above cannot catch a tag that is valid on one endpoint and
// dead on another: "genres" is real on the MusicBrainz artist lookup and absent
// from the artist search, so decoding it in the search silently yielded nothing
// while still appearing "covered". This check narrows the comparison to the
// endpoint each function actually calls, which is where that class of bug lives.
//
// Only structs declared inside the function body are checked; shared named types
// are covered by the union check, because they legitimately span endpoints.
var endpointContracts = []struct {
	receiver string
	function string
	captures []string
}{
	{receiver: "MusicBrainz", function: "SearchArtists", captures: []string{"musicbrainz_artist_search.json"}},
	{receiver: "MusicBrainz", function: "ResolveArtist", captures: []string{"musicbrainz_artist_lookup.json"}},
	{receiver: "MusicBrainz", function: "ResolveExternalArtist", captures: []string{"musicbrainz_url_artist_rels.json"}},
	{receiver: "MusicBrainz", function: "ArtistReleases", captures: []string{"musicbrainz_release_group_browse.json"}},
	{receiver: "MusicBrainz", function: "ArtistReleaseCredits", captures: []string{
		"musicbrainz_recording_search_credits.json",
		"musicbrainz_recording_search_dated.json",
		"musicbrainz_recording_search.json",
	}},
}

func captureKeys(t *testing.T, names []string) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, nested := range typed {
				keys[key] = true
				walk(nested)
			}
		case []any:
			for _, nested := range typed {
				walk(nested)
			}
		}
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join("testdata", "providers", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var payload any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		walk(payload)
	}
	return keys
}

// localStructTags returns the JSON tags of struct types declared inside the
// named method's body.
func localStructTags(t *testing.T, receiver, function string) ([]string, bool) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Name.Name != function || funcDecl.Recv == nil || funcDecl.Body == nil {
				continue
			}
			if receiverName(funcDecl) != receiver {
				continue
			}
			var tags []string
			ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
				structType, ok := node.(*ast.StructType)
				if !ok || structType.Fields == nil {
					return true
				}
				for _, field := range structType.Fields.List {
					if field.Tag == nil {
						continue
					}
					raw, err := strconv.Unquote(field.Tag.Value)
					if err != nil {
						continue
					}
					name := strings.Split(reflect.StructTag(raw).Get("json"), ",")[0]
					if name != "" && name != "-" {
						tags = append(tags, name)
					}
				}
				return true
			})
			return tags, true
		}
	}
	return nil, false
}

func receiverName(funcDecl *ast.FuncDecl) string {
	if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
		return ""
	}
	switch expr := funcDecl.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := expr.X.(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.Ident:
		return expr.Name
	}
	return ""
}

func TestResponseStructsMatchTheirOwnEndpointsPayload(t *testing.T) {
	for _, contract := range endpointContracts {
		t.Run(contract.receiver+"."+contract.function, func(t *testing.T) {
			tags, found := localStructTags(t, contract.receiver, contract.function)
			if !found {
				t.Fatalf("%s.%s no longer exists; update endpointContracts", contract.receiver, contract.function)
			}
			if len(tags) == 0 {
				t.Fatalf("%s.%s declares no response struct; the contract would pass vacuously",
					contract.receiver, contract.function)
			}
			keys := captureKeys(t, contract.captures)
			var missing []string
			for _, tag := range tags {
				if !keys[tag] {
					missing = append(missing, tag)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Fatalf("%s.%s decodes %v, which its own endpoint never sends (captured in %v).\n"+
					"A tag valid on a different endpoint is still dead here: decoding it yields nothing and the feature silently returns empty.",
					contract.receiver, contract.function, missing, contract.captures)
			}
		})
	}
}

// namedTypeContracts bind a package-level response type to every capture the
// endpoints that decode it produce.
//
// endpointContracts covers only structs declared inside a function body, so it
// reaches MusicBrainz and nothing else: iTunes and ListenBrainz decode through
// shared package-level types. That left the strongest check in this file bound
// entirely to one provider, while the testdata README names an iTunes instance
// of exactly the class it exists to catch.
//
// The comparison is against the union of that provider's captures rather than a
// single endpoint's, because a shared type legitimately spans endpoints -
// collectionArtistName is real on the song search and absent from the album
// lookup. That is narrower than the all-provider union check and catches the
// case that matters here: a tag that appears in no capture of its own provider.
var namedTypeContracts = []struct {
	typeName string
	captures []string
}{
	{typeName: "itunesResult", captures: []string{
		"itunes_artist_albums.json",
		"itunes_artist_search.json",
		"itunes_song_search.json",
		"itunes_compilation_song_search.json",
	}},
	{typeName: "ListenBrainzArtistStats", captures: []string{"listenbrainz_popularity_artist.json"}},
}

// TestNamedResponseTypesMatchTheirProviderPayloads closes the provider gap in
// the endpoint contract check.
func TestNamedResponseTypesMatchTheirProviderPayloads(t *testing.T) {
	for _, contract := range namedTypeContracts {
		t.Run(contract.typeName, func(t *testing.T) {
			tags := namedStructTags(t, contract.typeName)
			if len(tags) == 0 {
				t.Fatalf("%s declares no JSON tags; the contract would pass vacuously", contract.typeName)
			}
			keys := captureKeys(t, contract.captures)
			var missing []string
			for _, tag := range tags {
				if !keys[tag] {
					missing = append(missing, tag)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("%s decodes %v, which appear in none of its provider's captured payloads %v",
					contract.typeName, missing, contract.captures)
			}
		})
	}
}

// namedStructTags collects the JSON tags of a package-level struct type.
func namedStructTags(t *testing.T, typeName string) []string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok || spec.Name == nil || spec.Name.Name != typeName {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range structType.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json")
				name := strings.Split(tag, ",")[0]
				if name != "" && name != "-" {
					tags = append(tags, name)
				}
			}
			return false
		})
	}
	return tags
}
