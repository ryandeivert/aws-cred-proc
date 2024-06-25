/*
Proof-of-concept utility for use as an aws CLI credential_process
that performes aws CLI compatible caching/refreshing of credentials
*/

package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/mattn/go-tty"
	"github.com/yawn/ykoath"
)

var profile string
var noCache, mfaYK, forceRefresh, asVars bool
var duration time.Duration

func init() {
	const (
		usageProfile      = "the optional aws config profile to use for credentials. If left empty, either the current env will dictate the profile or \"default\" will be used"
		usageNoCache      = "disable caching credentials in the ~/.aws/cli/cache directory"
		usageDuration     = "duration for which these credentials will remain valid"
		usageYK           = "read MFA token from YubiKey versus prompting via stdin. Requires setting mfa_serial in the profile config, or the AWS_MFA_SERIAL env var"
		usageForceRefresh = "ignore any cached items and force a refresh of the credentials. The newly generated credentials will be cached for future use. To disable caching entirely, use the -no-cache flag"
		usageAsVars       = "format the items as environment variables for use in a shell"
		shorthandPrefix   = "shorthand for "
	)
	flag.StringVar(&profile, "profile", "", usageProfile)
	flag.StringVar(&profile, "p", "", shorthandPrefix+"-profile")
	flag.BoolVar(&noCache, "no-cache", false, usageNoCache)
	flag.BoolVar(&noCache, "n", false, shorthandPrefix+"-no-cache")
	flag.DurationVar(&duration, "duration", time.Minute*60, usageDuration)
	flag.DurationVar(&duration, "d", time.Minute*60, shorthandPrefix+"-duration")
	flag.BoolVar(&mfaYK, "mfa-yk", false, usageYK)
	flag.BoolVar(&mfaYK, "m", false, shorthandPrefix+"-mfa-yk")
	flag.BoolVar(&forceRefresh, "force-refresh", false, usageForceRefresh)
	flag.BoolVar(&forceRefresh, "f", false, shorthandPrefix+"-force-refresh")
	flag.BoolVar(&asVars, "variables", false, usageAsVars)
	flag.BoolVar(&asVars, "v", false, shorthandPrefix+"-variables")
}

type CLICache struct {
	provider     aws.CredentialsProvider
	cacheKey     computableCacheKey
	forceRefresh bool
	fullPath     string
}

func NewCache(provider aws.CredentialsProvider, forceRefresh bool, opts stscreds.AssumeRoleOptions) *CLICache {
	return &CLICache{
		provider:     provider,
		forceRefresh: forceRefresh,
		cacheKey: computableCacheKey{
			DurationSeconds: int(opts.Duration.Seconds()),
			ExternalId:      aws.ToString(opts.ExternalID),
			RoleArn:         opts.RoleARN,
			SerialNumber:    aws.ToString(opts.SerialNumber),
		},
	}
}

func (c *CLICache) pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (c *CLICache) path() string {
	if c.fullPath == "" {
		usr, err := user.Current()
		if err != nil {
			log.Fatal(err)
		}
		c.fullPath = filepath.Join(path.Join(usr.HomeDir, ".aws", "cli", "cache"), fmt.Sprintf("%s.json", c.cacheKey))
	}
	return c.fullPath
}

func (c *CLICache) Load(ctx context.Context) (aws.Credentials, error) {
	// Do not bother to check the cache if we're forcing a refresh
	if !c.forceRefresh {
		creds, err := c.get()
		if err == nil && !creds.Expired() {
			return creds, err // credentials are still valid
		}
	}

	// Fall back on the credential provider to get creds
	creds, err := c.provider.Retrieve(ctx)
	if err != nil {
		return creds, err
	}

	err = c.save(creds)
	if err != nil {
		return creds, err
	}

	return creds, nil
}

func (c *CLICache) get() (aws.Credentials, error) {

	creds := aws.Credentials{
		CanExpire: true, // The aws.Credentials.Expired() function needs this to be true
	}

	if !c.pathExists(c.path()) {
		return creds, fmt.Errorf("cache file does not exist")
	}

	data, err := os.ReadFile(c.path())
	if err != nil {
		return creds, fmt.Errorf("failed to read cache file, %w", err)
	}

	var v CLICompatCacheItem
	if err := json.Unmarshal(data, &v); err != nil {
		return creds, fmt.Errorf("failed to decode cache json, %w", err)
	}

	creds.AccessKeyID = v.Credentials.AccessKeyId
	creds.SecretAccessKey = v.Credentials.SecretAccessKey
	creds.SessionToken = v.Credentials.SessionToken
	creds.Expires = time.Time(v.Credentials.Expiration)

	return creds, nil
}

func (c *CLICache) save(creds aws.Credentials) error {

	// Ensure the cache directory exists
	dir := filepath.Dir(c.path())
	if c.pathExists(dir) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to make directories, %w", err)
		}
	}

	item := &CLICompatCacheItem{
		Credentials: &CachedCredentials{
			AccessKeyId:     creds.AccessKeyID,
			SecretAccessKey: creds.SecretAccessKey,
			SessionToken:    creds.SessionToken,
			Expiration:      ExpireTime(creds.Expires),
		},
	}

	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("failed to encode cache json, %w", err)
	}

	if err := os.WriteFile(c.path(), data, 0600); err != nil {
		return fmt.Errorf("failed to write cache file, %w", err)
	}

	return nil
}

type computableCacheKey struct {
	DurationSeconds int    `json:",omitempty"`
	ExternalId      string `json:",omitempty"`
	RoleArn         string `json:",omitempty"`
	SerialNumber    string `json:",omitempty"`
}

// Stringer function for computableCacheKey is a loose approximation of the botocore
// BaseAssumeRoleCredentialFetcher._create_cache_key function:
// https://github.com/boto/botocore/blob/69618a93752834ca99e52977058b5ee176df7007/botocore/credentials.py#L760-L780
// Additional json formatting is done to mimic the Python json format
func (v computableCacheKey) String() string {
	blob, err := json.Marshal(v)
	if err != nil {
		log.Fatal(err)
	}

	blobStr := string(blob)
	blobStr = strings.Replace(blobStr, `":`, `": `, -1)
	blobStr = strings.Replace(blobStr, `,"`, `, "`, -1)
	hash := sha1.New()
	if _, err := hash.Write([]byte(blobStr)); err != nil {
		log.Fatal(fmt.Errorf("failed to write hash, %w", err))
	}

	return strings.ToLower(hex.EncodeToString(hash.Sum(nil)))
}

type CLICompatCacheItem struct {
	Credentials *CachedCredentials
}

type CachedCredentials struct {
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string
	Expiration      ExpireTime
}

type ExpireTime time.Time

func (e ExpireTime) MarshalJSON() ([]byte, error) {
	s, err := json.Marshal(time.Time(e).Format("2006-01-02T15:04:05+00:00"))
	return s, err
}

func (e *ExpireTime) UnmarshalJSON(data []byte) error {
	v := strings.Trim(string(data), `"`)
	t, err := time.Parse("2006-01-02T15:04:05+00:00", v)
	if err != nil {
		return err
	}
	*e = ExpireTime(t)
	return nil
}

func TTYPrompt() (string, error) {
	tty, err := tty.Open()
	if err != nil {
		return "", err
	}
	defer tty.Close()

	fmt.Fprint(tty.Output(), "MFA Code: ")

	text, err := tty.ReadString()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(text), nil
}

func MFAYKCode(mfaSerial *string) func() (string, error) {
	return func() (string, error) {
		driver, err := ykoath.New()
		if err != nil {
			return "", err
		}

		_, err = driver.Select()

		return driver.Calculate(*mfaSerial, func(name string) error {
			// Using tty so the message does not get captured by awscli in stdout/stderr
			tty, err := tty.Open()
			if err != nil {
				return err
			}
			defer tty.Close()

			fmt.Fprint(tty.Output(), fmt.Sprintf("Please touch YubiKey now to generate MFA code for %q...\n", name))
			return nil
		})
	}
}

func NewProcessCredentials(creds aws.Credentials) *processcreds.CredentialProcessResponse {
	return &processcreds.CredentialProcessResponse{
		Version:         1,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      &creds.Expires,
	}
}

type shellCredentials struct {
	AWS_ACCESS_KEY_ID     string
	AWS_SECRET_ACCESS_KEY string
	AWS_SESSION_TOKEN     string
}

func NewShellCredentials(creds aws.Credentials) *shellCredentials {
	return &shellCredentials{
		AWS_ACCESS_KEY_ID:     creds.AccessKeyID,
		AWS_SECRET_ACCESS_KEY: creds.SecretAccessKey,
		AWS_SESSION_TOKEN:     creds.SessionToken,
	}
}

func (s *shellCredentials) String() string {
	ct := reflect.ValueOf(s).Elem()
	typeOfC := ct.Type()

	lines := make([]string, ct.NumField())
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		lines[i] = fmt.Sprintf("export %s=%v", strings.ToUpper(typeOfC.Field(i).Name), f)
	}
	return strings.Join(lines, "\n")
}

func writeToStdOut(v any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

func main() {
	flag.Parse()

	// The max session duration is 12 hours, so use that as the upper bound
	// IAM Roles can set their own max duration, but this is a sane default
	// and the assume role call will fail if a duration is set above the max
	if !(time.Minute*15 <= duration && duration <= time.Hour*12) {
		log.Fatal("duration must be between 15 minutes and 12 hours")
	}

	ctx := context.TODO()

	var opts stscreds.AssumeRoleOptions

	cfg, err := config.LoadDefaultConfig(
		ctx,
		// assume us-east-1 if no other region set
		config.WithDefaultRegion("us-east-1"),

		// optional profile name from ~/.aws/config
		// empty value will be ignored, falling back on environment variables, etc
		config.WithSharedConfigProfile(profile),

		config.WithAssumeRoleCredentialOptions(func(o *stscreds.AssumeRoleOptions) {
			// TTYPrompt is just an example here that allows you to enter the MFA token
			// without the input being captured by awscli (which captures stdin/stdout)
			// This could use a different token provider, like yubikey, etc
			// Note: a TokenProvider is required if mfa_serial is set (shared config, env, etc)
			o.TokenProvider = TTYPrompt
			if mfaYK {
				o.TokenProvider = MFAYKCode(o.SerialNumber)
			}
			o.Duration = duration
			opts = *o // Save these because we need them later
		}),
		config.WithCredentialsCacheOptions(func(o *aws.CredentialsCacheOptions) {
			o.ExpiryWindow = 5 * time.Minute // We could make this configurable or longer, but 5 minutes seems like a sane default
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	var loader aws.CredentialsProviderFunc
	if noCache {
		loader = cfg.Credentials.Retrieve
	} else {
		cache := NewCache(cfg.Credentials, forceRefresh, opts)
		loader = cache.Load
	}

	creds, err := loader(ctx)
	if err != nil {
		log.Fatal(err)
	}

	if asVars {
		_, err = fmt.Fprint(os.Stdout, NewShellCredentials(creds))
	} else {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(NewProcessCredentials(creds))
	}

	if err != nil {
		log.Fatal(err)
	}
}
