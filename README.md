# AWS CLI credential_process POC

This utility can be used with the [credential_process](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sourcing-external.html) setting of the AWS CLI to vend credentials with custom logic. It
supports caching of credentials that is compatible with the `aws` CLI, storing short-lived and refreshable
credentials in the `~/.aws/cli/cache` directory.

This, or a similar approach, can also be used to enable third-party providers for credentials, but more
importantly can be used to source MFA tokens from non-standard places - YubiKey, etc.

## Setup

1. Build the `aws-cred-proc` binary:
   ```shell
   make build
   ```

   Note: this will put the binary in your local user's `~/.aws/` directory, but you can place it wherever you wish.

2. Configure a role to be assumed. This will update your local `aws` CLI config file (`~/.aws/config`)

   This assumes you already have long-lived credentials configured in the `~/.aws/credentials` file. If you do not, run `aws configure` to add credentials before running the below commands.

   If `default` is not the profile you have configured, be sure to specify the appropriate source_profile in the first command.
   ```shell
   aws configure --profile cp-role set source_profile default
   aws configure --profile cp-role set role_arn arn:aws:iam::123456789012:role/<ROLE-NAME>
   aws configure --profile cp-role set mfa_serial arn:aws:iam::210987654321:mfa/<MFA-NAME>
   ```

   You may also set additonal configuration options, like the `role_session_name`:
   ```shell
   aws configure --profile cp-role set role_session_name $(whoami)
   ```

   See the AWS Command Line Interface User Guide for all [available options](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html).

3. Add a "dummy" profile that is only responsible for executing the `credential_process`. Here the profile name `cred-proc` is used:
   ```shell
   aws configure --profile cred-proc set credential_process $HOME/.aws/aws-cred-proc
   ```

## Usage
1. Set your "target" profile as an environment variable. If you chose a different profile name above, be sure to use the right value here.
   ```shell
   export AWS_PROFILE=cp-role
   ```

2. Execute the `aws` CLI using the "dummy" profile you set above (`cred-proc`).

   The `aws` CLI will use this profile, but the `credential_process` itself will use the `AWS_PROFILE` environment variable setting when loading credentials.

   You will be prompted for an MFA token. Once entered, the credentials will be cached and only refreshed when they are close to expiring.
   ```shell
   aws --profile cred-proc sts get-caller-identity
   MFA Code: ######
   {
       "UserId": "AROA#################:aws-go-sdk-1718770578433481000",
       "Account": "123456789012",
       "Arn": "arn:aws:sts::123456789012:assumed-role/<ROLE-NAME>/aws-go-sdk-1718770578433481000"
   }
   ```

   The target profile can also be supplied directly in-line when using the `aws` CLI, like so:
   ```shell
   AWS_PROFILE=cp-role aws --profile cred-proc sts get-caller-identity
   ```

3. Re-running the above command (or any other `aws` command) will reuse the cached credentials - try it!

## MFA with YubiKey

If you have added your AWS MFA secret to your YubiKey, you can read it with the `credential_process` using the `-m` flag:

```shell
aws configure --profile cred-proc-yk set credential_process "$HOME/.aws/aws-cred-proc --mfa-yk"
```

Now, using this new profile will look like this:

```shell
aws --profile cred-proc-yk sts get-caller-identity
Please touch YubiKey now to generate MFA code for "arn:aws:iam::210987654321:mfa/<MFA-NAME>"...
{
    "UserId": "AROA#################:aws-go-sdk-1718775214321169000",
    "Account": "123456789012",
    "Arn": "arn:aws:sts::123456789012:assumed-role/<ROLE-NAME>/aws-go-sdk-1718775214321169000"
}
```

## Credentials as Environment Variables

If you'd to use the generated credentials with a tool other than the `aws` CLI, it's more sensible to have them
set as environment variables. This is because AWS SDKs will not load credentials from the `~/.aws/cli/cache`
directory. You can achieve this by using `eval` and calling the `aws-cred-proc` utility directly, specifying
the `--variables` flag:

```shell
eval $($HOME/.aws/aws-cred-proc --profile cp-role --variables)
```

You will then have `AWS_*` environment variables set in your current shell session, and can veryfy with `env | grep AWS_`.


## Full Usage

```
Usage aws-cred-proc:
  -d duration
    	shorthand for -duration (default 1h0m0s)
  -duration duration
    	duration for which these credentials will remain valid (default 1h0m0s)
  -f	shorthand for -force-refresh
  -force-refresh
    	ignore any cached items and force a refresh of the credentials. The newly generated credentials will be cached for future use. To disable caching entirely, use the -no-cache flag
  -m	shorthand for -mfa-yk
  -mfa-yk
    	read MFA token from YubiKey versus prompting via stdin. Requires setting mfa_serial in the profile config, or the AWS_MFA_SERIAL env var
  -n	shorthand for -no-cache
  -no-cache
    	disable caching credentials in the ~/.aws/cli/cache directory
  -p string
    	shorthand for -profile
  -profile string
    	the optional aws config profile to use for credentials. If left empty, either the current env will dictate the profile or "default" will be used
  -v	shorthand for -variables
  -variables
    	format the items as environment variables for use in a shell
```