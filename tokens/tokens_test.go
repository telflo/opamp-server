package tokens

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/google/uuid"
)

// The fixtures (testdata/README): a key setting with a non-ASCII character,
// a bearer made with it under Node's WebCrypto, and one this server's Go
// made with it.
var (
	nodeClaims = Claims{
		TokenID: uuid.MustParse("11111111-2222-4333-8444-555555555555"),
		FleetID: uuid.MustParse("66666666-7777-4888-9999-000000000000"),
	}
	goClaims = Claims{
		TokenID: uuid.MustParse("aaaaaaaa-2222-4333-8444-555555555555"),
		FleetID: uuid.MustParse("bbbbbbbb-7777-4888-9999-000000000000"),
	}
)

const (
	currentKey  = "the-current-encryption-key-0123456789abcdef"
	previousKey = "a-previous-encryption-key-0123456789abcdef"
	olderKey    = "an-older-encryption-key-0123456789abcdef-xyz"
	strangerKey = "a-key-this-server-was-never-given-0123456789"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(raw), "\r\n")
}

func keyring(t *testing.T, current string, previous ...string) *Keyring {
	t.Helper()
	k, err := NewKeyring(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(k.Destroy)
	return k
}

// webCryptoDecrypt is the WebCrypto side, step by step: the key is
// crypto.subtle.digest("SHA-256", TextEncoder().encode(setting)), imported
// raw for AES-GCM; the bearer is atob'd; the first 12 bytes are the iv, the
// rest the ciphertext with its 128-bit tag (WebCrypto's default tagLength),
// no additional data.
func webCryptoDecrypt(setting, bearer string) ([]byte, error) {
	keyBytes := sha256.Sum256([]byte(setting)) // TextEncoder is UTF-8
	raw, err := base64.StdEncoding.DecodeString(bearer)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keyBytes[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithTagSize(block, 16)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, raw[:12], raw[12:], nil)
}

// webCryptoEncrypt is the WebCrypto side's encryption: a random 12-byte iv,
// AES-GCM with a 128-bit tag, iv ‖ ciphertext, btoa.
func webCryptoEncrypt(t *testing.T, setting, plaintext string) string {
	t.Helper()
	keyBytes := sha256.Sum256([]byte(setting))
	block, err := aes.NewCipher(keyBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithTagSize(block, 16)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))
}

// A bearer made under Node's WebCrypto decrypts here with the same key,
// accepted as a previous key as well as the current one.
func TestATelfloAppBearerDecrypts(t *testing.T) {
	setting, bearer := fixture(t, "fixture-key.txt"), fixture(t, "node-bearer.txt")
	if !strings.ContainsRune(setting, 'ÿ') {
		t.Fatal("the fixture key lost its non-ASCII character")
	}
	for _, k := range []*Keyring{keyring(t, setting), keyring(t, currentKey, previousKey, setting)} {
		claims, err := k.Read(bearer)
		if err != nil || claims != nodeClaims {
			t.Fatalf("Read(the Node bearer) = %+v, %v; want %+v", claims, err, nodeClaims)
		}
	}
	if _, err := keyring(t, currentKey, previousKey).Read(bearer); !errors.Is(err, ErrInvalidBearer) {
		t.Fatalf("without its key, Read = %v; want ErrInvalidBearer", err)
	}
}

// The other direction: a bearer this server's Go made (the committed
// fixture, and a fresh one) decrypts by WebCrypto's steps, and its
// plaintext is exactly the bearer format's JSON.
func TestAGoBearerDecryptsByTelfloAppsAlgorithm(t *testing.T) {
	setting := fixture(t, "fixture-key.txt")
	plaintext, err := webCryptoDecrypt(setting, fixture(t, "go-bearer.txt"))
	if err != nil || string(plaintext) != `{"token_id":"aaaaaaaa-2222-4333-8444-555555555555","deployment_id":"bbbbbbbb-7777-4888-9999-000000000000"}` {
		t.Fatalf("the committed Go bearer by WebCrypto's steps = %q, %v", plaintext, err)
	}
	if claims, err := keyring(t, setting).Read(fixture(t, "go-bearer.txt")); err != nil || claims != goClaims {
		t.Fatalf("Read(the Go bearer) = %+v, %v", claims, err)
	}

	minted, err := keyring(t, setting, previousKey).Mint(goClaims)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err = webCryptoDecrypt(setting, minted)
	if err != nil || string(plaintext) != string(goClaims.Marshal()) {
		t.Fatalf("a minted bearer by WebCrypto's steps = %q, %v", plaintext, err)
	}
	// And WebCrypto's encryption of the format's JSON reads here.
	theirs := webCryptoEncrypt(t, setting, `{"token_id":"`+nodeClaims.TokenID.String()+`","deployment_id":"`+nodeClaims.FleetID.String()+`"}`)
	if claims, err := keyring(t, currentKey, setting).Read(theirs); err != nil || claims != nodeClaims {
		t.Fatalf("Read(a WebCrypto bearer) = %+v, %v", claims, err)
	}
}

// The plaintext is the bearer format's JSON exactly: its two keys, in order,
// the ids in lower case.
func TestClaimsMarshalTelfloAppsJSON(t *testing.T) {
	claims := Claims{TokenID: uuid.MustParse("ABCDEF01-2222-4333-8444-555555555555"), FleetID: nodeClaims.FleetID}
	want := `{"token_id":"abcdef01-2222-4333-8444-555555555555","deployment_id":"66666666-7777-4888-9999-000000000000"}`
	if got := string(claims.Marshal()); got != want {
		t.Fatalf("Marshal = %s, want %s", got, want)
	}
}

// Mint then Read gives the claims back; each mint has a fresh nonce, so two
// bearers of one token differ and both read.
func TestMintAndReadRoundTrip(t *testing.T) {
	k := keyring(t, currentKey, previousKey)
	claims := Claims{TokenID: uuid.New(), FleetID: uuid.New()}
	first, err := k.Mint(claims)
	if err != nil {
		t.Fatal(err)
	}
	second, err := k.Mint(claims)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two mints of one token are the same bearer: the nonce isn't fresh")
	}
	for _, bearer := range []string{first, second} {
		if got, err := k.Read(bearer); err != nil || got != claims {
			t.Fatalf("Read = %+v, %v; want %+v", got, err, claims)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(first)
	if len(raw) != 12+len(claims.Marshal())+16 {
		t.Fatalf("the bearer is %d bytes, want nonce + plaintext + tag", len(raw))
	}
}

// Every accepted key opens a bearer sealed under it; Mint uses the current
// one only; a key never configured opens nothing.
func TestEveryAcceptedKeyAndOnlyThoseRead(t *testing.T) {
	k := keyring(t, currentKey, previousKey, olderKey)
	claims := Claims{TokenID: uuid.New(), FleetID: uuid.New()}
	for _, setting := range []string{currentKey, previousKey, olderKey} {
		bearer := webCryptoEncrypt(t, setting, string(claims.Marshal()))
		if got, err := k.Read(bearer); err != nil || got != claims {
			t.Errorf("a bearer under an accepted key: Read = %+v, %v", got, err)
		}
	}
	stranger := webCryptoEncrypt(t, strangerKey, string(claims.Marshal()))
	if _, err := k.Read(stranger); !errors.Is(err, ErrInvalidBearer) {
		t.Fatalf("a bearer under a key never configured: Read = %v, want ErrInvalidBearer", err)
	}
	minted, err := k.Mint(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := webCryptoDecrypt(currentKey, minted); err != nil {
		t.Fatalf("Mint didn't use the current key: %v", err)
	}
	for _, setting := range []string{previousKey, olderKey} {
		if _, err := webCryptoDecrypt(setting, minted); err == nil {
			t.Fatal("Mint used a previous key")
		}
	}
	// A keyring never reads a bearer under a key it doesn't hold.
	if _, err := keyring(t, olderKey).Read(webCryptoEncrypt(t, currentKey, string(claims.Marshal()))); !errors.Is(err, ErrInvalidBearer) {
		t.Fatal("a keyring read a bearer under a key it doesn't hold")
	}
}

// Any changed byte, in the nonce, the ciphertext or the tag, is refused, as
// is a bearer cut short or made longer, or that isn't base64.
func TestATamperedBearerIsRefused(t *testing.T) {
	k := keyring(t, currentKey)
	bearer, err := k.Mint(Claims{TokenID: uuid.New(), FleetID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(bearer)
	if !strings.HasSuffix(bearer, "=") {
		t.Fatal("the bearer has no padding, so the case without it would prove nothing")
	}
	for i := range raw {
		tampered := append([]byte(nil), raw...)
		tampered[i] ^= 0x01
		if _, err := k.Read(base64.StdEncoding.EncodeToString(tampered)); !errors.Is(err, ErrInvalidBearer) {
			t.Fatalf("byte %d of %d flipped: Read = %v, want ErrInvalidBearer", i, len(raw), err)
		}
	}
	for label, bad := range map[string]string{
		"empty":            "",
		"cut short":        base64.StdEncoding.EncodeToString(raw[:len(raw)-1]),
		"only a nonce":     base64.StdEncoding.EncodeToString(raw[:12]),
		"a byte appended":  base64.StdEncoding.EncodeToString(append(append([]byte(nil), raw...), 0)),
		"not base64":       "!!" + bearer,
		"base64url":        strings.NewReplacer("+", "-", "/", "_").Replace(bearer) + "-_",
		"without padding":  strings.TrimRight(bearer, "="),
		"the Bearer word":  "Bearer " + bearer,
		"with a space":     bearer[:10] + " " + bearer[10:],
		"twice":            bearer + bearer,
		"a different blob": base64.StdEncoding.EncodeToString(make([]byte, len(raw))),
	} {
		if _, err := k.Read(bad); !errors.Is(err, ErrInvalidBearer) {
			t.Errorf("%s: Read = %v, want ErrInvalidBearer", label, err)
		}
	}
}

// A sealed plaintext that isn't exactly a token's claims is refused even
// under an accepted key: a missing, empty, null or extra field, or an id
// that isn't a UUID.
func TestAPlaintextThatIsNotClaimsIsRefused(t *testing.T) {
	k := keyring(t, currentKey)
	id := uuid.NewString()
	for label, plaintext := range map[string]string{
		"no deployment_id":         `{"token_id":"` + id + `"}`,
		"no token_id":              `{"deployment_id":"` + id + `"}`,
		"deployment_id empty":      `{"token_id":"` + id + `","deployment_id":""}`,
		"deployment_id null":       `{"token_id":"` + id + `","deployment_id":null}`,
		"token_id a number":        `{"token_id":1,"deployment_id":"` + id + `"}`,
		"an extra field":           `{"token_id":"` + id + `","deployment_id":"` + id + `","organization_id":"` + id + `"}`,
		"deployment_id not a UUID": `{"token_id":"` + id + `","deployment_id":"fleet-1"}`,
		"a UUID in braces":         `{"token_id":"{` + id + `}","deployment_id":"` + id + `"}`,
		"a UUID without hyphens":   `{"token_id":"` + strings.ReplaceAll(id, "-", "") + `","deployment_id":"` + id + `"}`,
		"the old fleet_id name":    `{"token_id":"` + id + `","fleet_id":"` + id + `"}`,
		"not JSON":                 `token ` + id,
		"an array":                 `["` + id + `","` + id + `"]`,
		"null":                     `null`,
		"an empty object":          `{}`,
		"a trailing value":         `{"token_id":"` + id + `","deployment_id":"` + id + `"} {}`,
	} {
		bearer := webCryptoEncrypt(t, currentKey, plaintext)
		if _, err := k.Read(bearer); !errors.Is(err, ErrInvalidBearer) {
			t.Errorf("%s: Read = %v, want ErrInvalidBearer", label, err)
		}
	}
	// Upper-case hex is a UUID still (Postgres's uuid reads it).
	upper := webCryptoEncrypt(t, currentKey, `{"token_id":"`+strings.ToUpper(id)+`","deployment_id":"`+id+`"}`)
	if claims, err := k.Read(upper); err != nil || claims.TokenID.String() != id {
		t.Fatalf("an upper-case id: %+v, %v", claims, err)
	}
}

// A destroyed keyring (the keys purged on exit) mints and reads nothing,
// and never runs AES on an empty key.
func TestADestroyedKeyringMintsAndReadsNothing(t *testing.T) {
	k, err := NewKeyring(currentKey, []string{previousKey})
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := k.Mint(goClaims)
	if err != nil {
		t.Fatal(err)
	}
	k.Destroy()
	if _, err := k.Mint(goClaims); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Mint after Destroy = %v, want ErrNoKey", err)
	}
	if _, err := k.Read(bearer); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Read after Destroy = %v, want ErrNoKey", err)
	}
	if _, err := Open(nil, bearer); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Open with no keys = %v, want ErrNoKey", err)
	}
}

// A keyring needs a current key, and no empty previous one.
func TestNewKeyringRefusesAnEmptyKey(t *testing.T) {
	if _, err := NewKeyring("", nil); err == nil {
		t.Fatal("a keyring without a current key")
	}
	if _, err := NewKeyring(currentKey, []string{previousKey, ""}); err == nil {
		t.Fatal("a keyring with an empty previous key")
	}
	k := keyring(t, currentKey, previousKey, olderKey)
	if k.Count() != 3 || len(k.Keys()) != 3 {
		t.Fatalf("Count = %d, Keys = %d; want 3", k.Count(), len(k.Keys()))
	}
}

// The keys are frozen, read-only buffers of 32 bytes, the setting's SHA-256.
func TestDeriveKeyIsTheSettingsSHA256Frozen(t *testing.T) {
	key := DeriveKey(currentKey)
	defer key.Destroy()
	want := sha256.Sum256([]byte(currentKey))
	if !key.IsAlive() || key.IsMutable() || string(key.Bytes()) != string(want[:]) {
		t.Fatalf("DeriveKey: alive %v, mutable %v, 32 bytes of the digest %v", key.IsAlive(), key.IsMutable(), string(key.Bytes()) == string(want[:]))
	}
}

// The bytes a keyring is derived from are zeroed once hashed, accepted or
// refused, and the keys derived from them are the settings' (a bearer
// minted by a keyring of the same strings reads).
func TestNewKeyringFromBytesWipesTheSettings(t *testing.T) {
	zeroed := func(b []byte) bool {
		for _, c := range b {
			if c != 0 {
				return false
			}
		}
		return true
	}
	current, previous := []byte(currentKey), []byte(previousKey)
	k, err := NewKeyringFromBytes(current, [][]byte{previous})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Destroy()
	if !zeroed(current) || !zeroed(previous) {
		t.Fatalf("the settings' bytes survived: %q, %q", current, previous)
	}
	same := keyring(t, currentKey, previousKey)
	bearer, err := k.Mint(goClaims)
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := same.Read(bearer); err != nil || claims != goClaims {
		t.Fatalf("a keyring of the same settings read %+v, %v", claims, err)
	}

	refused := []byte(currentKey)
	if _, err := NewKeyringFromBytes(refused, [][]byte{{}}); err == nil || !zeroed(refused) {
		t.Fatalf("a refused keyring: %v, the current key's bytes zeroed %v", err, zeroed(refused))
	}
	single := []byte(olderKey)
	key := DeriveKeyFromBytes(single)
	defer key.Destroy()
	if want := sha256.Sum256([]byte(olderKey)); !zeroed(single) || string(key.Bytes()) != string(want[:]) {
		t.Fatalf("DeriveKeyFromBytes: zeroed %v, the digest %v", zeroed(single), string(key.Bytes()) == string(want[:]))
	}
}

// Every request shares the buffers: concurrent mints and reads under -race,
// across the current and a previous key, all succeed.
func TestConcurrentMintsAndReads(t *testing.T) {
	k := keyring(t, currentKey, previousKey)
	previous := []*memguard.LockedBuffer{k.Keys()[1]}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 64 {
		wg.Go(func() {
			for range 50 {
				claims := Claims{TokenID: uuid.New(), FleetID: uuid.New()}
				bearer, err := k.Mint(claims)
				if g%2 == 1 {
					bearer, err = Seal(previous[0], claims.Marshal())
				}
				if err != nil {
					errs <- err
					return
				}
				if got, err := k.Read(bearer); err != nil || got != claims {
					errs <- errors.New("a concurrent read failed")
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
