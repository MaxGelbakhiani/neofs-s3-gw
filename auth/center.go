package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/hex"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/klauspost/compress/zstd"
	"github.com/nspcc-dev/neofs-api-go/refs"
	"github.com/nspcc-dev/neofs-api-go/service"
	crypto "github.com/nspcc-dev/neofs-crypto"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var authorizationFieldRegexp = regexp.MustCompile(`AWS4-HMAC-SHA256 Credential=(?P<access_key_id>[^/]+)/(?P<date>[^/]+)/(?P<region>[^/]*)/(?P<service>[^/]+)/aws4_request, SignedHeaders=(?P<signed_header_fields>.*), Signature=(?P<v4_signature>.*)`)

const emptyStringSHA256 = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`

// Center is a central app's authentication/authorization management unit.
type Center struct {
	log         *zap.Logger
	submatcher  *regexpSubmatcher
	zstdEncoder *zstd.Encoder
	zstdDecoder *zstd.Decoder
	neofsKeys   struct {
		PrivateKey *ecdsa.PrivateKey
		PublicKey  *ecdsa.PublicKey
	}
	ownerID      refs.OwnerID
	wifString    string
	userAuthKeys struct {
		PrivateKey *rsa.PrivateKey
		PublicKey  *rsa.PublicKey
	}
}

// NewCenter creates an instance of AuthCenter.
func NewCenter(log *zap.Logger) (*Center, error) {
	zstdEncoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create zstd encoder")
	}
	zstdDecoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create zstd decoder")
	}
	return &Center{
		log:         log,
		submatcher:  &regexpSubmatcher{re: authorizationFieldRegexp},
		zstdEncoder: zstdEncoder,
		zstdDecoder: zstdDecoder,
	}, nil
}

func (center *Center) SetNeoFSKeys(key *ecdsa.PrivateKey) error {
	publicKey := &key.PublicKey
	oid, err := refs.NewOwnerID(publicKey)
	if err != nil {
		return errors.Wrap(err, "failed to get OwnerID")
	}
	center.neofsKeys.PrivateKey = key
	wif, err := crypto.WIFEncode(key)
	if err != nil {
		return errors.Wrap(err, "failed to get WIF string from given key")
	}
	center.neofsKeys.PublicKey = publicKey
	center.ownerID = oid
	center.wifString = wif
	return nil
}

func (center *Center) GetNeoFSPrivateKey() *ecdsa.PrivateKey {
	return center.neofsKeys.PrivateKey
}

func (center *Center) GetNeoFSPublicKey() *ecdsa.PublicKey {
	return center.neofsKeys.PublicKey
}

func (center *Center) GetOwnerID() refs.OwnerID {
	return center.ownerID
}

func (center *Center) GetWIFString() string {
	return center.wifString
}

func (center *Center) SetUserAuthKeys(key *rsa.PrivateKey) {
	center.userAuthKeys.PrivateKey = key
	center.userAuthKeys.PublicKey = &key.PublicKey
}

func (center *Center) packBearerToken(bearerToken *service.BearerTokenMsg) (string, string, error) {
	data, err := bearerToken.Marshal()
	if err != nil {
		return "", "", errors.Wrap(err, "failed to marshal bearer token")
	}
	encryptedKeyID, err := encrypt(center.userAuthKeys.PublicKey, center.compress(data))
	if err != nil {
		return "", "", errors.Wrap(err, "failed to encrypt bearer token bytes")
	}
	accessKeyID := hex.EncodeToString(encryptedKeyID)
	secretAccessKey := hex.EncodeToString(sha256Hash(data))
	return accessKeyID, secretAccessKey, nil
}

func (center *Center) unpackBearerToken(accessKeyID string) (*service.BearerTokenMsg, string, error) {
	encryptedKeyID, err := hex.DecodeString(accessKeyID)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to decode HEX string")
	}
	compressedKeyID, err := decrypt(center.userAuthKeys.PrivateKey, encryptedKeyID)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to decrypt key ID")
	}
	data, err := center.decompress(compressedKeyID)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to decompress key ID")
	}
	bearerToken := new(service.BearerTokenMsg)
	if err := bearerToken.Unmarshal(data); err != nil {
		return nil, "", errors.Wrap(err, "failed to unmarshal embedded bearer token")
	}
	secretAccessKey := hex.EncodeToString(sha256Hash(data))
	return bearerToken, secretAccessKey, nil
}

func (center *Center) AuthenticationPassed(request *http.Request) (*service.BearerTokenMsg, error) {
	queryValues := request.URL.Query()
	if queryValues.Get("X-Amz-Algorithm") == "AWS4-HMAC-SHA256" {
		return nil, errors.New("pre-signed form of request is not supported")
	}
	authHeaderField := request.Header["Authorization"]
	if len(authHeaderField) != 1 {
		return nil, errors.New("unsupported request: wrong length of Authorization header field")
	}
	sms1 := center.submatcher.getSubmatches(authHeaderField[0])
	if len(sms1) != 6 {
		return nil, errors.New("bad Authorization header field")
	}
	signedHeaderFieldsNames := strings.Split(sms1["signed_header_fields"], ";")
	if len(signedHeaderFieldsNames) == 0 {
		return nil, errors.New("wrong format of signed headers part")
	}
	signatureDateTime, err := time.Parse("20060102T150405Z", request.Header.Get("X-Amz-Date"))
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse x-amz-date header field")
	}
	accessKeyID := sms1["access_key_id"]
	bearerToken, secretAccessKey, err := center.unpackBearerToken(accessKeyID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unpack bearer token")
	}
	otherRequest := request.Clone(context.TODO())
	otherRequest.Header = map[string][]string{}
	for hfn, hfvs := range request.Header {
		for _, shfn := range signedHeaderFieldsNames {
			if strings.EqualFold(hfn, shfn) {
				otherRequest.Header[hfn] = hfvs
			}
		}
	}
	awsCreds := credentials.NewStaticCredentials(accessKeyID, secretAccessKey, "")
	signer := v4.NewSigner(awsCreds)
	body, err := readAndKeepBody(request)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read out request body")
	}
	_, err = signer.Sign(otherRequest, body, sms1["service"], sms1["region"], signatureDateTime)
	if err != nil {
		return nil, errors.Wrap(err, "failed to sign temporary HTTP request")
	}
	sms2 := center.submatcher.getSubmatches(otherRequest.Header.Get("Authorization"))
	if sms1["v4_signature"] != sms2["v4_signature"] {
		return nil, errors.Wrap(err, "failed to pass authentication procedure")
	}
	return bearerToken, nil
}

// TODO: Make this write into a smart buffer backed by a file on a fast drive.
func readAndKeepBody(request *http.Request) (*bytes.Reader, error) {
	if request.Body == nil {
		var r bytes.Reader
		return &r, nil
	}
	payload, err := ioutil.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	request.Body = ioutil.NopCloser(bytes.NewReader(payload))
	return bytes.NewReader(payload), nil
}

func (center *Center) compress(data []byte) []byte {
	return center.zstdEncoder.EncodeAll(data, make([]byte, 0, len(data)))
}

func (center *Center) decompress(data []byte) ([]byte, error) {
	return center.zstdDecoder.DecodeAll(data, nil)
}
