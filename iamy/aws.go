package iamy

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/pkg/errors"
)

// AwsFetcher fetches account data from AWS
type AwsFetcher struct {
	// As Policy and Role descriptions are immutable, we can skip fetching them
	// when pushing to AWS
	SkipFetchingPolicyAndRoleDescriptions bool
	HeuristicCfnMatching                  bool
	SkipTagged                            []string
	IncludeTagged                         []string
	SkipPathPrefixes                      []string

	Debug *log.Logger

	iam     *iamClient
	s3      *s3Client
	cfn     *cfnClient
	tagging *resourceGroupsTaggingAPIClient
	account *Account
	data    AccountData

	descriptionFetchWaitGroup sync.WaitGroup
	descriptionFetchError     error
	policyTagFetchError       error
}

func (a *AwsFetcher) init() error {
	var err error

	s := awsSession()
	a.iam = newIamClient(s)
	a.s3 = newS3Client(s)
	a.cfn = newCfnClient(s)
	a.tagging = newResourceGroupsTaggingAPIClient(s)

	if a.account, err = a.getAccount(); err != nil {
		return err
	}
	a.data = AccountData{
		Account: a.account,
	}

	return nil
}

// Fetch queries AWS for account data
func (a *AwsFetcher) Fetch() (*AccountData, error) {
	if err := a.init(); err != nil {
		return nil, errors.Wrap(err, "Error in init")
	}

	if !a.HeuristicCfnMatching {
		log.Println("Fetching CFN data")
		if err := a.cfn.PopulateMangedResourceData(); err != nil {
			return nil, errors.Wrap(err, "Error fetching CFN data")
		}
	}

	var wg sync.WaitGroup
	var iamErr, s3Err error

	log.Println("Fetching IAM data")
	wg.Add(1)
	go func() {
		defer wg.Done()
		iamErr = a.fetchIamData()
	}()

	log.Println("Fetching S3 data")
	wg.Add(1)
	go func() {
		defer wg.Done()
		s3Err = a.fetchS3Data()
	}()

	wg.Wait()

	if iamErr != nil {
		return nil, errors.Wrap(iamErr, "Error fetching IAM data")
	}
	if s3Err != nil {
		return nil, errors.Wrap(s3Err, "Error fetching S3 data")
	}

	return &a.data, nil
}

func (a *AwsFetcher) fetchS3Data() error {
	buckets, err := a.s3.listAllBuckets()
	if err != nil {
		return errors.Wrap(err, "Error listing buckets")
	}
	for _, b := range buckets {
		if b.policyJson == "" {
			continue
		}
		if ok, err := a.isSkippableManagedResource(CfnS3Bucket, b.name, b.tags, "__DONTSKIPS3__"); ok {
			log.Printf(err)
			continue
		}

		policyDoc, err := NewPolicyDocumentFromJson(b.policyJson)
		if err != nil {
			return errors.Wrap(err, "Error creating Policy document")
		}

		bp := BucketPolicy{
			BucketName: b.name,
			Policy:     policyDoc,
		}

		a.data.BucketPolicies = append(a.data.BucketPolicies, &bp)
	}

	return nil
}

func (a *AwsFetcher) fetchIamData() error {
	var populateIamDataErr error
	var populateInstanceProfileErr error
	err := a.iam.GetAccountAuthorizationDetailsPages(
		&iam.GetAccountAuthorizationDetailsInput{
			Filter: aws.StringSlice([]string{
				iam.EntityTypeUser,
				iam.EntityTypeGroup,
				iam.EntityTypeRole,
				iam.EntityTypeLocalManagedPolicy,
			}),
		},
		func(resp *iam.GetAccountAuthorizationDetailsOutput, lastPage bool) bool {
			populateIamDataErr = a.populateIamData(resp)
			if populateIamDataErr != nil {
				return false
			}
			return true
		},
	)
	if populateIamDataErr != nil {
		return err
	}
	if err != nil {
		return err
	}
	// Fetch instance profiles
	err = a.iam.ListInstanceProfilesPages(&iam.ListInstanceProfilesInput{},
		func(resp *iam.ListInstanceProfilesOutput, lastPage bool) bool {
			populateInstanceProfileErr = a.populateInstanceProfileData(resp)
			if populateInstanceProfileErr != nil {
				return false
			}
			return true
		})
	if populateInstanceProfileErr != nil {
		return err
	}
	if err != nil {
		return err
	}
	return nil
}

func (a *AwsFetcher) populateInlinePolicies(source []*iam.PolicyDetail, target *[]InlinePolicy) error {
	for _, ip := range source {
		doc, err := NewPolicyDocumentFromEncodedJson(*ip.PolicyDocument)
		if err != nil {
			return err
		}
		*target = append(*target, InlinePolicy{
			Name:   *ip.PolicyName,
			Policy: doc,
		})
	}

	return nil
}

func (a *AwsFetcher) marshalPolicyDescriptionAsync(policyArn string, target *string) {
	a.descriptionFetchWaitGroup.Add(1)
	go func() {
		defer a.descriptionFetchWaitGroup.Done()
		log.Println("Fetching policy description for", policyArn)

		var err error
		*target, err = a.iam.getPolicyDescription(policyArn)
		if err != nil {
			a.descriptionFetchError = err
		}
	}()
}

func (a *AwsFetcher) fetchPolicyTags(policyArn string, tags *map[string]string) {
	log.Println("Fetching tags for", policyArn)

	var err error
	*tags, err = a.iam.getPolicyTags(policyArn)
	if err != nil {
		a.policyTagFetchError = err
	}
}

func (a *AwsFetcher) marshalRoleAsync(roleName string, roleDescription *string, roleMaxSessionDuration *int) {
	a.descriptionFetchWaitGroup.Add(1)
	go func() {
		defer a.descriptionFetchWaitGroup.Done()
		log.Println("Fetching role description for", roleName)

		var err error
		var sessionDuration int
		*roleDescription, sessionDuration, err = a.iam.getRole(roleName)
		if sessionDuration > 0 {
			*roleMaxSessionDuration = sessionDuration
		}
		if err != nil {
			a.descriptionFetchError = err
		}
	}()
}

func (a *AwsFetcher) populateInstanceProfileData(resp *iam.ListInstanceProfilesOutput) error {
	for _, profileResp := range resp.InstanceProfiles {
		if ok, err := a.isSkippableManagedResource(CfnInstanceProfile, *profileResp.InstanceProfileName, map[string]string{}, *profileResp.Path); ok {
			log.Printf(err)
			continue
		}

		profile := InstanceProfile{iamService: iamService{
			Name: *profileResp.InstanceProfileName,
			Path: *profileResp.Path,
		}}
		for _, roleResp := range profileResp.Roles {
			role := *(roleResp.RoleName)
			profile.Roles = append(profile.Roles, role)
		}
		a.data.InstanceProfiles = append(a.data.InstanceProfiles, &profile)
	}
	return nil
}

func (a *AwsFetcher) populateIamData(resp *iam.GetAccountAuthorizationDetailsOutput) error {
	for _, userResp := range resp.UserDetailList {
		tags := make(map[string]string)
		for _, tag := range userResp.Tags {
			tags[*tag.Key] = *tag.Value
		}

		if ok, err := a.isSkippableManagedResource(CfnIamUser, *userResp.UserName, tags, *userResp.Path); ok {
			log.Printf(err)
			continue
		}

		user := User{
			iamService: iamService{
				Name: *userResp.UserName,
				Path: *userResp.Path,
			},
		}

		for _, g := range userResp.GroupList {
			user.Groups = append(user.Groups, *g)
		}
		for _, p := range userResp.AttachedManagedPolicies {
			user.Policies = append(user.Policies, a.account.normalisePolicyArn(*p.PolicyArn))
		}
		if err := a.populateInlinePolicies(userResp.UserPolicyList, &user.InlinePolicies); err != nil {
			return err
		}
		user.Tags = tags

		a.data.Users = append(a.data.Users, &user)
	}

	for _, groupResp := range resp.GroupDetailList {
		if ok, err := a.isSkippableManagedResource(CfnIamGroup, *groupResp.GroupName, map[string]string{}, *groupResp.Path); ok {
			log.Printf(err)
			continue
		}

		group := Group{iamService: iamService{
			Name: *groupResp.GroupName,
			Path: *groupResp.Path,
		}}

		for _, p := range groupResp.AttachedManagedPolicies {
			group.Policies = append(group.Policies, a.account.normalisePolicyArn(*p.PolicyArn))
		}
		if err := a.populateInlinePolicies(groupResp.GroupPolicyList, &group.InlinePolicies); err != nil {
			return err
		}

		a.data.Groups = append(a.data.Groups, &group)
	}

	for _, roleResp := range resp.RoleDetailList {
		tags := make(map[string]string)
		for _, tag := range roleResp.Tags {
			tags[*tag.Key] = *tag.Value
		}

		if ok, err := a.isSkippableManagedResource(CfnIamRole, *roleResp.RoleName, tags, *roleResp.Path); ok {
			log.Printf(err)
			continue
		}

		role := Role{iamService: iamService{
			Name: *roleResp.RoleName,
			Path: *roleResp.Path,
		}}

		if !a.SkipFetchingPolicyAndRoleDescriptions {
			a.marshalRoleAsync(*roleResp.RoleName, &role.Description, &role.MaxSessionDuration)
		}

		var err error
		role.AssumeRolePolicyDocument, err = NewPolicyDocumentFromEncodedJson(*roleResp.AssumeRolePolicyDocument)
		if err != nil {
			return err
		}
		for _, p := range roleResp.AttachedManagedPolicies {
			role.Policies = append(role.Policies, a.account.normalisePolicyArn(*p.PolicyArn))
		}
		if err := a.populateInlinePolicies(roleResp.RolePolicyList, &role.InlinePolicies); err != nil {
			return err
		}

		a.data.addRole(&role)
	}

	policyArns := make([]*string, 0)
	for _, policyResp := range resp.Policies {
		policyArns = append(policyArns, policyResp.Arn)
	}

	policyTags, err := a.tagging.getMultiplePolicyTags(policyArns)
	if err != nil {
		log.Printf("Error: %v", err)
		return err
	}

	for _, policyResp := range resp.Policies {
		if ok, err := a.isSkippableManagedResource(CfnIamPolicy, *policyResp.PolicyName, map[string]string{}, *policyResp.Path); ok {
			log.Printf(err)
			continue
		}

		defaultPolicyVersion := findDefaultPolicyVersion(policyResp.PolicyVersionList)
		doc, err := NewPolicyDocumentFromEncodedJson(*defaultPolicyVersion.Document)
		if err != nil {
			return err
		}

		p := Policy{
			iamService: iamService{
				Name: *policyResp.PolicyName,
				Path: *policyResp.Path,
			},
			oldestVersionId:      findOldestPolicyVersionId(policyResp.PolicyVersionList),
			numberOfVersions:     len(policyResp.PolicyVersionList),
			nondefaultVersionIds: findNonDefaultPolicyVersionIds(policyResp.PolicyVersionList),
			Policy:               doc,
		}

		if !a.SkipFetchingPolicyAndRoleDescriptions {
			a.marshalPolicyDescriptionAsync(*policyResp.Arn, &p.Description)
		}
		if tags, ok := policyTags[*policyResp.Arn]; ok {
			p.Tags = tags
		} else {
			p.Tags = make(map[string]string)
		}
		// Need to do this _after_ we fetch tags for the Policy
		if ok, err := a.isSkippableManagedResource(CfnIamPolicy, *policyResp.PolicyName, p.Tags, *policyResp.Path); ok {
			log.Printf(err)
			continue
		}

		a.data.addPolicy(&p)
	}

	a.descriptionFetchWaitGroup.Wait()

	return a.descriptionFetchError
}

func findDefaultPolicyVersion(versions []*iam.PolicyVersion) *iam.PolicyVersion {
	for _, version := range versions {
		if *version.IsDefaultVersion {
			return version
		}
	}
	panic("Expected a default policy version")
}

func findNonDefaultPolicyVersionIds(versions []*iam.PolicyVersion) []string {
	ss := []string{}
	for _, version := range versions {
		if !*version.IsDefaultVersion {
			ss = append(ss, *version.VersionId)
		}
	}
	return ss
}

func findOldestPolicyVersionId(versions []*iam.PolicyVersion) string {
	oldest := versions[0]
	for _, version := range versions[1:] {
		if version.CreateDate.Before(*oldest.CreateDate) {
			oldest = version
		}
	}
	return *oldest.VersionId
}

func (a *AwsFetcher) getAccount() (*Account, error) {
	var err error
	acct := Account{}

	acct.Id, err = GetAwsAccountId(awsSession(), a.Debug)
	if err == aws.ErrMissingRegion {
		return nil, errors.New("Error determining the AWS account id - check the AWS_REGION environment variable is set")
	}
	if err != nil {
		return nil, err
	}

	aliasResp, err := a.iam.ListAccountAliases(&iam.ListAccountAliasesInput{})
	if err != nil {
		return nil, err
	}
	if len(aliasResp.AccountAliases) > 0 {
		acct.Alias = *aliasResp.AccountAliases[0]
	}

	return &acct, nil
}

// isSkippableResource takes the resource identifier as a string and
// checks it against known resources that we shouldn't need to manage as
// it will already be managed by another process (such as Cloudformation
// roles).
//
// Returns a boolean of whether it can be skipped and a string of the
// reasoning why it was skipped.

func (a *AwsFetcher) isSkippableManagedResource(cfnType CfnResourceType, resourceIdentifier string, tags map[string]string, resourcePath string) (bool, string) {
	if len(a.IncludeTagged) > 0 {
		for _, tag := range a.IncludeTagged {
			if _, ok := tags[tag]; ok {
				return false, ""
			}
		}
	}

	for _, tag := range a.SkipTagged {
		if stackName, ok := tags[tag]; ok {
			return true, fmt.Sprintf("Skipping resource %s tagged with %s in stack %s", resourceIdentifier, tag, stackName)
		}
	}

	for _, path := range a.SkipPathPrefixes {
		if strings.HasPrefix(resourcePath, path) {
			return true, fmt.Sprintf("Skipping resource %s with path %s matches %s", resourceIdentifier, resourcePath, path)
		}
	}

	if a.cfn.IsManagedResource(cfnType, resourceIdentifier) {
		return true, fmt.Sprintf("CloudFormation generated resource %s", resourceIdentifier)
	}

	if strings.Contains(resourceIdentifier, "AWSServiceRole") || strings.Contains(resourceIdentifier, "aws-service-role") {
		return true, fmt.Sprintf("AWS Service role generated resource %s", resourceIdentifier)
	}

	return false, ""
}
