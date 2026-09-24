package rclonestore

import (
	"errors"
	"reflect"
	"testing"
)

// TestParseRemoteMatchesRclone pins parseRemote to rclone's own connection-string
// state machine (fs/fspath/path.go Parse, rclone v1.71.1): a quote opens a quoted
// value ONLY as the value's first byte (anywhere else it is literal); inside a
// quoted value the active quote is escaped by doubling; after the closing quote
// only ':' ',' or another quote may follow; parameter names are [0-9A-Za-z_.]+; a
// bare "name" parameter has no value. The first spec-ending ':' splits the path.
func TestParseRemoteMatchesRclone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		remote  string
		backend string
		spec    string
		path    string
		params  []remoteParam
		wantErr bool
	}{
		{remote: ":local:", backend: "local", spec: "local", path: ""},
		{remote: ":local:/data", backend: "local", spec: "local", path: "/data"},
		{
			remote: ":s3,provider=Minio,endpoint='http://h:9':bkt/p", backend: "s3",
			spec: "s3,provider=Minio,endpoint='http://h:9'", path: "bkt/p",
			params: []remoteParam{{name: "provider", raw: "Minio", value: "Minio"}, {name: "endpoint", raw: "'http://h:9'", value: "http://h:9"}},
		},
		{
			// Gate M3: interior quotes are literal, so these are TWO values.
			remote: ":local,description=ab'SECRETONE,links=NOTA'BOOLSECRET:/r", backend: "local",
			spec: "local,description=ab'SECRETONE,links=NOTA'BOOLSECRET", path: "/r",
			params: []remoteParam{{name: "description", raw: "ab'SECRETONE", value: "ab'SECRETONE"}, {name: "links", raw: "NOTA'BOOLSECRET", value: "NOTA'BOOLSECRET"}},
		},
		{
			// Gate A4: the spec ends at the first unquoted ':' after an unquoted value.
			remote: ":local,description=it's:/tmp/o'x:y", backend: "local",
			spec: "local,description=it's", path: "/tmp/o'x:y",
			params: []remoteParam{{name: "description", raw: "it's", value: "it's"}},
		},
		{
			remote: `:local,a='it''s',b="say ""hi""",c='x"y':/p`, backend: "local",
			spec: `local,a='it''s',b="say ""hi""",c='x"y'`, path: "/p",
			params: []remoteParam{
				{name: "a", raw: "'it''s'", value: "it's"},
				{name: "b", raw: `"say ""hi"""`, value: `say "hi"`},
				{name: "c", raw: `'x"y'`, value: `x"y`},
			},
		},
		{
			remote: ":local,links,description=X,empty=:/d", backend: "local",
			spec: "local,links,description=X,empty=", path: "/d",
			params: []remoteParam{{name: "links"}, {name: "description", raw: "X", value: "X"}, {name: "empty"}},
		},
		{
			remote: ":s3,endpoint=http://minio:9000,provider=Minio:/bucket", backend: "s3",
			spec: "s3,endpoint=http", path: "//minio:9000,provider=Minio:/bucket",
			params: []remoteParam{{name: "endpoint", raw: "http", value: "http"}},
		},
		{remote: ":local,Dotted.Name_1=v:/p", backend: "local", spec: "local,Dotted.Name_1=v", path: "/p",
			params: []remoteParam{{name: "Dotted.Name_1", raw: "v", value: "v"}}},
		{remote: ":local,a='x'y:/p", wantErr: true},
		{remote: ":local,a='x'y':/p", wantErr: true},     // text after a closing quote, even if another quote follows      // only , : or a quote after a closing quote
		{remote: ":local,a='x:/p", wantErr: true},        // unterminated quoted value
		{remote: ":local,a=x", wantErr: true},            // value never ends
		{remote: ":local,=x:/p", wantErr: true},          // empty parameter name
		{remote: ":local,,a=x:/p", wantErr: true},        // empty parameter name
		{remote: ":local,a-b=x:/p", wantErr: true},       // bad parameter name byte
		{remote: ":local,a", wantErr: true},              // parameter never ends
		{remote: ":loc/al:/p", wantErr: true},            // path separator in the backend
		{remote: "::x", wantErr: true},                   // empty backend
		{remote: ":", wantErr: true},                     // no backend
		{remote: ":LOCAL:/p", wantErr: true},             // backend grammar (lowercase alnum)
		{remote: ":local,a=\"x\"\"y:/p", wantErr: true},  // doubled quote leaves the value open
		{remote: ":local,a=x'y',b='z:/p", wantErr: true}, // b's quote never closes
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.remote, func(t *testing.T) {
			t.Parallel()
			got, err := parseRemote(tt.remote)
			if tt.wantErr {
				var oe *OptionsError
				if !errors.As(err, &oe) {
					t.Fatalf("parseRemote = %+v, %v; want *OptionsError", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRemote: %v", err)
			}
			want := parsedRemote{connectionString: true, backend: tt.backend, spec: tt.spec, path: tt.path, params: tt.params}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parseRemote =\n  %+v\nwant\n  %+v", got, want)
			}
		})
	}
}
