package channels

import (
	"strings"
	"testing"
)

func TestChannelTypeValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		typeValue ChannelType
		want      bool
	}{
		{name: "temporary", typeValue: ChannelTypeTemporary, want: true},
		{name: "semi-permanent", typeValue: ChannelTypeSemiPermanent, want: true},
		{name: "permanent", typeValue: ChannelTypePermanent, want: true},
		{name: "negative", typeValue: -1},
		{name: "above range", typeValue: ChannelTypePermanent + 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.typeValue.Valid(); got != test.want {
				t.Errorf("ChannelType(%d).Valid() = %t, want %t", test.typeValue, got, test.want)
			}
		})
	}
}

func TestChannelSpecValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		spec        ChannelSpec
		wantErrText string
	}{
		{
			name: "valid temporary",
			spec: ChannelSpec{Name: "Lobby", Type: ChannelTypeTemporary},
		},
		{
			name: "valid permanent with optional fields",
			spec: ChannelSpec{
				Name: "Music", Type: ChannelTypePermanent, ParentID: 7,
				MaxClients: -1, NeededJoinPower: 42, OpusBitrate: 64_000,
			},
		},
		{
			name:        "empty name",
			spec:        ChannelSpec{Type: ChannelTypePermanent},
			wantErrText: "channel name must not be empty",
		},
		{
			name:        "invalid type",
			spec:        ChannelSpec{Name: "Broken", Type: 99},
			wantErrText: "invalid channel type 99",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.spec.Validate()
			if test.wantErrText == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErrText) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantErrText)
			}
		})
	}
}
