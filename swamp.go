package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

func die(msg string, err error) {
	fmt.Fprintf(os.Stderr, msg + ": %v\n", err)
	os.Exit(1)
}

func getCallerId(svc *sts.STS) *sts.GetCallerIdentityOutput {
	output, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		die("Error fetching caller id", err)
	}

	return output
}

func getTokenCode(tokenSerialNumber string) *string {
	if tokenSerialNumber == "" {
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Enter mfa token for %s: ", tokenSerialNumber)
	if tokenCode, err := reader.ReadString('\n'); err != nil {
		die("Error reading mfa token", err)
		return nil
	} else {
		tokenCode = strings.Trim(tokenCode, " \r\n")
		return &tokenCode
	}
}

func validateSessionToken(options session.Options) bool {
	sess := session.Must(session.NewSessionWithOptions(options))
	svc := sts.New(sess)
	_, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	return err == nil
}

func getSessionToken(options session.Options, config *SwampConfig) *sts.Credentials {
	sess := session.Must(session.NewSessionWithOptions(options))
	svc := sts.New(sess)
	output, err := svc.GetSessionToken(&sts.GetSessionTokenInput{
		DurationSeconds: &config.intermediateDuration,
		SerialNumber:    &config.tokenSerialNumber,
		TokenCode:       getTokenCode(config.tokenSerialNumber),
	})
	if err != nil {
		die("Error getting session token", err)
	}

	return output.Credentials
}

// validate session token and request a new one if it's invalid.
// write target profile into .aws/credentials
func ensureSessionTokenProfile(config *SwampConfig, pw *ProfileWriter) {
	if validateSessionToken(session.Options{
		Config:  aws.Config{Region: &config.region},
		Profile: config.intermediateProfile,
	}) {
		fmt.Printf("Session token for profile %s is still valid\n", config.profile)
	} else {
		cred := getSessionToken(session.Options{
			Config:  aws.Config{Region: &config.region},
			Profile: config.profile,
		}, config)
		pw.writeProfile(cred, &config.intermediateProfile, &config.region)
	}
}

func assumeRole(svc *sts.STS, roleArn, roleSessionName *string, duration *int64) *sts.Credentials {
	output, err := svc.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         roleArn,
		RoleSessionName: roleSessionName,
		DurationSeconds: duration,
	})
	if err != nil {
		die("Error assuming role", err)
	}

	return output.Credentials
}

// assume-role into target account and write target profile into .aws/credentials
func ensureTargetProfile(config *SwampConfig, pw *ProfileWriter, sess *session.Session) {
	svc := sts.New(sess)

	userId := getCallerId(svc).Arn
	parts := strings.Split(*userId, "/")
	roleSessionName := parts[len(parts) - 1]

	cred := assumeRole(svc, config.GetRoleArn(), &roleSessionName, &config.targetDuration)
	pw.writeProfile(cred, &config.targetProfile, sess.Config.Region)
}

func writeProfileToFile(config *SwampConfig) {
	file, err := os.Create(config.exportFile)
	if err != nil {
		die("Error writing target profile to export file", err)
	}
	defer file.Close()

	fmt.Fprintf(file, "export AWS_PROFILE=%s\nunset AWS_ACCESS_KEY_ID\nunset AWS_SECRET_ACCESS_KEY\n", config.targetProfile)
}

func main() {
	// set up command line flags
	config := NewSwampConfig()
	config.SetupFlags()
	flag.Parse()

	// check user input on command line flags
	baseProfile := &config.profile
	config.Validate()

	if config.tokenSerialNumber != "" {
		baseProfile = &config.intermediateProfile
	}

	pw := NewProfileWriter()
	for {
		if config.tokenSerialNumber != "" {
			// get intermediate session token with mfa, use that to assume role into target account
			ensureSessionTokenProfile(config, pw)
		}

		var sess *session.Session
		if config.useInstanceProfile {
			sess = session.Must(session.NewSession())
		} else {
			sess = session.Must(session.NewSessionWithOptions(session.Options{
				Config:  aws.Config{Region: &config.region},
				Profile: *baseProfile, }))
		}

		ensureTargetProfile(config, pw, sess)

		if config.exportProfile {
			writeProfileToFile(config)
		}

		if !config.renew {
			break
		}
		time.Sleep(time.Second * time.Duration(config.targetDuration / 2))
	}
}
