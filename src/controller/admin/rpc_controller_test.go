package admin

import (
	"errors"
	"testing"

	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/GMWalletApp/epusdt/util/constant"
)

func TestValidateRpcNodeURLForTypeOkx(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr error
	}{
		{
			name:   "accepts zero placeholder",
			rawURL: "https://web3.okx.com/zh-hans/explorer/bsc/address/{0}/token-transfer",
		},
		{
			name:   "accepts named placeholder",
			rawURL: "https://web3.okx.com/zh-hans/explorer/{chain}/address/{address}/token-transfer",
		},
		{
			name:    "rejects http",
			rawURL:  "http://web3.okx.com/zh-hans/explorer/bsc/address/{0}/token-transfer",
			wantErr: constant.RpcNodeHTTPURLErr,
		},
		{
			name:    "rejects missing placeholder",
			rawURL:  "https://web3.okx.com/zh-hans/explorer/bsc/address/static/token-transfer",
			wantErr: constant.RpcNodeURLErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRpcNodeURLForType(tt.rawURL, mdb.RpcNodeTypeOkx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateRpcNodeURLForType() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestRpcNodeAPIKeyFromRequestAllowsEmptyOkxKey(t *testing.T) {
	got, err := rpcNodeAPIKeyFromRequest("   ", mdb.RpcNodeTypeOkx)
	if err != nil {
		t.Fatalf("rpcNodeAPIKeyFromRequest() error = %v", err)
	}
	if got != "" {
		t.Fatalf("api key = %q, want empty", got)
	}
}
