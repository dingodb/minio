/*
 * MinIO Cloud Storage, (C) 2018-2019 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/minio/minio-go/v7/pkg/set"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/auth"
	iampolicy "github.com/minio/minio/pkg/iam/policy"
	"github.com/minio/minio/pkg/madmin"
)

// UsersSysType - defines the type of users and groups system that is
// active on the server.
type UsersSysType string

// Types of users configured in the server.
const (
	// This mode uses the internal users system in MinIO.
	MinIOUsersSysType UsersSysType = "MinIOUsersSys"

	// This mode uses users and groups from a configured LDAP
	// server.
	LDAPUsersSysType UsersSysType = "LDAPUsersSys"
)

const (
	// IAM configuration directory.
	iamConfigPrefix = minioConfigPrefix + "/iam"

	// IAM users directory.
	iamConfigUsersPrefix = iamConfigPrefix + "/users/"

	// IAM service accounts directory.
	iamConfigServiceAccountsPrefix = iamConfigPrefix + "/service-accounts/"

	// IAM groups directory.
	iamConfigGroupsPrefix = iamConfigPrefix + "/groups/"

	// IAM policies directory.
	iamConfigPoliciesPrefix = iamConfigPrefix + "/policies/"

	// IAM sts directory.
	iamConfigSTSPrefix = iamConfigPrefix + "/sts/"

	// IAM Policy DB prefixes.
	iamConfigPolicyDBPrefix                = iamConfigPrefix + "/policydb/"
	iamConfigPolicyDBUsersPrefix           = iamConfigPolicyDBPrefix + "users/"
	iamConfigPolicyDBSTSUsersPrefix        = iamConfigPolicyDBPrefix + "sts-users/"
	iamConfigPolicyDBServiceAccountsPrefix = iamConfigPolicyDBPrefix + "service-accounts/"
	iamConfigPolicyDBGroupsPrefix          = iamConfigPolicyDBPrefix + "groups/"

	// IAM identity file which captures identity credentials.
	iamIdentityFile = "identity.json"

	// IAM policy file which provides policies for each users.
	iamPolicyFile = "policy.json"

	// IAM group members file
	iamGroupMembersFile = "members.json"

	// IAM format file
	iamFormatFile = "format.json"

	iamFormatVersion1 = 1
)

const (
	statusEnabled  = "enabled"
	statusDisabled = "disabled"
)

type iamFormat struct {
	Version int `json:"version"`
}

func newIAMFormatVersion1() iamFormat {
	return iamFormat{Version: iamFormatVersion1}
}

func getIAMFormatFilePath() string {
	return iamConfigPrefix + SlashSeparator + iamFormatFile
}

func getUserIdentityPath(user string, userType IAMUserType) string {
	var basePath string
	switch userType {
	case srvAccUser:
		basePath = iamConfigServiceAccountsPrefix
	case stsUser:
		basePath = iamConfigSTSPrefix
	default:
		basePath = iamConfigUsersPrefix
	}
	return pathJoin(basePath, user, iamIdentityFile)
}

func getGroupInfoPath(group string) string {
	return pathJoin(iamConfigGroupsPrefix, group, iamGroupMembersFile)
}

func getPolicyDocPath(name string) string {
	return pathJoin(iamConfigPoliciesPrefix, name, iamPolicyFile)
}

func getMappedPolicyPath(name string, userType IAMUserType, isGroup bool) string {
	if isGroup {
		return pathJoin(iamConfigPolicyDBGroupsPrefix, name+".json")
	}
	switch userType {
	case srvAccUser:
		return pathJoin(iamConfigPolicyDBServiceAccountsPrefix, name+".json")
	case stsUser:
		return pathJoin(iamConfigPolicyDBSTSUsersPrefix, name+".json")
	default:
		return pathJoin(iamConfigPolicyDBUsersPrefix, name+".json")
	}
}

// UserIdentity represents a user's secret key and their status
type UserIdentity struct {
	Version     int              `json:"version"`
	Credentials auth.Credentials `json:"credentials"`
}

func newUserIdentity(cred auth.Credentials) UserIdentity {
	return UserIdentity{Version: 1, Credentials: cred}
}

// GroupInfo contains info about a group
type GroupInfo struct {
	Version int      `json:"version"`
	Status  string   `json:"status"`
	Members []string `json:"members"`
}

func newGroupInfo(members []string) GroupInfo {
	return GroupInfo{Version: 1, Status: statusEnabled, Members: members}
}

// MappedPolicy represents a policy name mapped to a user or group
type MappedPolicy struct {
	Version  int    `json:"version"`
	Policies string `json:"policy"`
}

// converts a mapped policy into a slice of distinct policies
func (mp MappedPolicy) toSlice() []string {
	var policies []string
	for _, policy := range strings.Split(mp.Policies, ",") {
		policy = strings.TrimSpace(policy)
		if policy == "" {
			continue
		}
		policies = append(policies, policy)
	}
	return policies
}

func (mp MappedPolicy) policySet() set.StringSet {
	var policies []string
	for _, policy := range strings.Split(mp.Policies, ",") {
		policy = strings.TrimSpace(policy)
		if policy == "" {
			continue
		}
		policies = append(policies, policy)
	}
	return set.CreateStringSet(policies...)
}

func newMappedPolicy(policy string) MappedPolicy {
	return MappedPolicy{Version: 1, Policies: policy}
}

// IAMSys - config system.
type IAMSys struct {
	sync.Mutex

	usersSysType UsersSysType

	// map of policy names to policy definitions
	iamPolicyDocsMap map[string]iampolicy.Policy
	// map of usernames to credentials
	iamUsersMap map[string]auth.Credentials
	// map of group names to group info
	iamGroupsMap map[string]GroupInfo
	// map of user names to groups they are a member of
	iamUserGroupMemberships map[string]set.StringSet
	// map of usernames/temporary access keys to policy names
	iamUserPolicyMap map[string]MappedPolicy
	// map of group names to policy names
	iamGroupPolicyMap map[string]MappedPolicy

	// Persistence layer for IAM subsystem
	store IAMStorageAPI

	// configLoaded will be closed and remain so after first load.
	configLoaded chan struct{}
}

// IAMUserType represents a user type inside MinIO server
type IAMUserType int

const (
	regularUser IAMUserType = iota
	stsUser
	srvAccUser
)

// key options
type options struct {
	ttl int64 //expiry in seconds
}

// IAMStorageAPI defines an interface for the IAM persistence layer
type IAMStorageAPI interface {
	lock()
	unlock()

	rlock()
	runlock()

	migrateBackendFormat(context.Context) error

	loadPolicyDoc(ctx context.Context, policy string, m map[string]iampolicy.Policy) error
	loadPolicyDocs(ctx context.Context, m map[string]iampolicy.Policy) error

	getUserCredentials(ctx context.Context, user string, userType IAMUserType) (auth.Credentials, error)
	loadUser(ctx context.Context, user string, userType IAMUserType, m map[string]auth.Credentials) error
	loadUsers(ctx context.Context, userType IAMUserType, m map[string]auth.Credentials) error

	getGroupInfo(ctx context.Context, group string) (GroupInfo, error)
	loadGroup(ctx context.Context, group string, m map[string]GroupInfo) error
	loadGroups(ctx context.Context, m map[string]GroupInfo) error

	getMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool) (MappedPolicy, error)
	loadMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool, m map[string]MappedPolicy) error
	loadMappedPolicies(ctx context.Context, userType IAMUserType, isGroup bool, m map[string]MappedPolicy) error

	loadAll(context.Context, *IAMSys) error

	saveIAMConfig(ctx context.Context, item interface{}, path string, opts ...options) error
	loadIAMConfig(ctx context.Context, item interface{}, path string) error
	deleteIAMConfig(ctx context.Context, path string) error

	savePolicyDoc(ctx context.Context, policyName string, p iampolicy.Policy) error
	saveMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool, mp MappedPolicy, opts ...options) error
	saveUserIdentity(ctx context.Context, name string, userType IAMUserType, u UserIdentity, opts ...options) error
	saveGroupInfo(ctx context.Context, group string, gi GroupInfo) error

	deletePolicyDoc(ctx context.Context, policyName string) error
	deleteMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool) error
	deleteUserIdentity(ctx context.Context, name string, userType IAMUserType) error
	deleteGroupInfo(ctx context.Context, name string) error
	newNSLock(bucket string, objects ...string) RWLocker
	watch(context.Context, *IAMSys)
}

// LoadGroup - loads a specific group from storage, and updates the
// memberships cache. If the specified group does not exist in
// storage, it is removed from in-memory maps as well - this
// simplifies the implementation for group removal. This is called
// only via IAM notifications.
func (sys *IAMSys) LoadGroup(group string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	gi, err := sys.store.getGroupInfo(context.Background(), group)
	if err != nil && !errors.Is(err, errNoSuchGroup) {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	if errors.Is(err, errNoSuchGroup) {
		// group does not exist - so remove from memory.
		sys.removeGroupFromMembershipsMap(group)
		delete(sys.iamGroupsMap, group)
		delete(sys.iamGroupPolicyMap, group)
		return nil
	}
	sys.iamGroupsMap[group] = gi
	// Updating the group memberships cache happens in two steps:
	//
	// 1. Remove the group from each user's list of memberships.
	// 2. Add the group to each member's list of memberships.
	//
	// This ensures that regardless of members being added or
	// removed, the cache stays current.

	sys.removeGroupFromMembershipsMap(group)
	sys.updateGroupMembershipsMap(group, &gi)
	return nil
}

// LoadPolicy - reloads a specific canned policy from backend disks or etcd.
func (sys *IAMSys) LoadPolicy(policyName string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	return sys.store.loadPolicyDoc(context.Background(), policyName, sys.iamPolicyDocsMap)
}

func (sys *IAMSys) LoadMappedPolicies(isGroup bool) error {
	m := make(map[string]MappedPolicy)
	if err := sys.store.loadMappedPolicies(context.Background(), regularUser, isGroup, m); err != nil {
		return err
	}
	sys.Lock()
	defer sys.Unlock()
	if isGroup {
		sys.iamGroupPolicyMap = m
	} else {
		sys.iamUserPolicyMap = m
	}
	return nil
}

// LoadPolicyMapping - loads the mapped policy for a user or group
// from storage into server memory.
func (sys *IAMSys) LoadPolicyMapping(userOrGroup string, userType IAMUserType, isGroup bool) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	p, err := sys.store.getMappedPolicy(context.Background(), userOrGroup, userType, isGroup)
	// Ignore policy not mapped error
	if err != nil && !errors.Is(err, errNoSuchPolicy) {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	if isGroup {
		sys.iamGroupPolicyMap[userOrGroup] = p
	} else {
		sys.iamUserPolicyMap[userOrGroup] = p
	}
	return nil
}

// LoadUser - reloads a specific user from backend disks or etcd.
func (sys *IAMSys) LoadUser(accessKey string, userType IAMUserType) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}
	var err error
	var user auth.Credentials
	if user, err = sys.store.getUserCredentials(context.Background(), accessKey, userType); err != nil {
		return err
	}

	// Ignore policy not mapped error
	var p MappedPolicy
	if p, err = sys.store.getMappedPolicy(context.Background(), accessKey, userType, false); err != nil && !errors.Is(err, errNoSuchPolicy) {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	sys.iamUsersMap[accessKey] = user
	sys.iamUserPolicyMap[accessKey] = p
	return nil
}

func (sys *IAMSys) LoadAllTypeUsers() error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	m := make(map[string]auth.Credentials)
	for _, iamUserType := range []IAMUserType{regularUser, stsUser, srvAccUser} {
		if err := sys.store.loadUsers(context.Background(), iamUserType, m); err != nil {
			return err
		}
	}
	sys.Lock()
	defer sys.Unlock()
	sys.iamUsersMap = m
	return nil
}

// LoadServiceAccount - reloads a specific service account from backend disks or etcd.
func (sys *IAMSys) LoadServiceAccount(accessKey string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if globalEtcdClient == nil {
		err := sys.store.loadUser(context.Background(), accessKey, srvAccUser, sys.iamUsersMap)
		if err != nil {
			return err
		}
	}
	// When etcd is set, we use watch APIs so this code is not needed.
	return nil
}

// Perform IAM configuration migration.
func (sys *IAMSys) doIAMConfigMigration(ctx context.Context) error {
	return sys.store.migrateBackendFormat(ctx)
}

// InitStore initializes IAM stores
func (sys *IAMSys) InitStore(objAPI ObjectLayer) {
	sys.Lock()
	defer sys.Unlock()

	if globalEtcdClient == nil {
		sys.store = newIAMObjectStore(objAPI)
	}

	if globalLDAPConfig.Enabled {
		sys.EnableLDAPSys()
	}
}

// Initialized check if IAM is initialized
func (sys *IAMSys) Initialized() bool {
	if sys == nil {
		return false
	}
	sys.Lock()
	defer sys.Unlock()
	return sys.store != nil
}

// Load - loads all credentials
func (sys *IAMSys) Load(ctx context.Context, store IAMStorageAPI) error {
	iamUsersMap := make(map[string]auth.Credentials)
	iamGroupsMap := make(map[string]GroupInfo)
	iamUserPolicyMap := make(map[string]MappedPolicy)
	iamGroupPolicyMap := make(map[string]MappedPolicy)
	iamPolicyDocsMap := make(map[string]iampolicy.Policy)

	store.rlock()
	defer store.runlock()

	isMinIOUsersSys := sys.usersSysType == MinIOUsersSysType
	if err := store.loadPolicyDocs(ctx, iamPolicyDocsMap); err != nil && !errors.As(err, &BucketNotFound{}) {
		return err
	}
	// Sets default canned policies, if none are set.
	setDefaultCannedPolicies(iamPolicyDocsMap)

	if isMinIOUsersSys {
		if err := store.loadUsers(ctx, regularUser, iamUsersMap); err != nil && !errors.As(err, &BucketNotFound{}) {
			return err
		}
		if err := store.loadGroups(ctx, iamGroupsMap); err != nil && !errors.As(err, &BucketNotFound{}) {
			return err
		}
	}

	// load polices mapped to users
	if err := store.loadMappedPolicies(ctx, regularUser, false, iamUserPolicyMap); err != nil && !errors.As(err, &BucketNotFound{}) {
		return err
	}

	// load policies mapped to groups
	if err := store.loadMappedPolicies(ctx, regularUser, true, iamGroupPolicyMap); err != nil && !errors.As(err, &BucketNotFound{}) {
		return err
	}

	if err := store.loadUsers(ctx, srvAccUser, iamUsersMap); err != nil && !errors.As(err, &BucketNotFound{}) {
		return err
	}

	// load STS temp users
	if err := store.loadUsers(ctx, stsUser, iamUsersMap); err != nil && !errors.As(err, &BucketNotFound{}) {
		return err
	}

	// load STS policy mappings
	if err := store.loadMappedPolicies(ctx, stsUser, false, iamUserPolicyMap); err != nil && !errors.As(err, &BucketNotFound{}) {
		return err
	}

	sys.Lock()
	defer sys.Unlock()

	sys.iamPolicyDocsMap = iamPolicyDocsMap

	sys.iamUsersMap = iamUsersMap

	sys.iamUserPolicyMap = iamUserPolicyMap

	// purge any expired entries which became expired now.
	var expiredEntries []string
	for k, v := range sys.iamUsersMap {
		if v.IsExpired() {
			delete(sys.iamUsersMap, k)
			delete(sys.iamUserPolicyMap, k)
			expiredEntries = append(expiredEntries, k)
			// Deleting on the disk is taken care of in the next cycle
		}
	}

	for _, v := range sys.iamUsersMap {
		if v.IsServiceAccount() {
			for _, accessKey := range expiredEntries {
				if v.ParentUser == accessKey {
					_ = store.deleteUserIdentity(ctx, v.AccessKey, srvAccUser)
					delete(sys.iamUsersMap, v.AccessKey)
				}
			}
		}
	}

	// purge any expired entries which became expired now.
	for k, v := range sys.iamUsersMap {
		if v.IsExpired() {
			delete(sys.iamUsersMap, k)
			delete(sys.iamUserPolicyMap, k)
			// Deleting on the etcd is taken care of in the next cycle
		}
	}

	sys.iamGroupPolicyMap = iamGroupPolicyMap

	sys.iamGroupsMap = iamGroupsMap

	sys.buildUserGroupMemberships()
	select {
	case <-sys.configLoaded:
	default:
		close(sys.configLoaded)
	}
	return nil
}

// Init - initializes config system by reading entries from config/iam
func (sys *IAMSys) Init(ctx context.Context, objAPI ObjectLayer) {
	// Initialize IAM store
	sys.InitStore(objAPI)

	retryCtx, cancel := context.WithCancel(ctx)

	// Indicate to our routine to exit cleanly upon return.
	defer cancel()

	// Hold the lock for migration only.
	txnLk := objAPI.NewNSLock(MinioMetaBucket, MinioMetaLockFile)

	// allocate dynamic timeout once before the loop
	iamLockTimeout := NewDynamicTimeout(5*time.Second, 3*time.Second)

	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	if os.Getenv("JUICEFS_META_READ_ONLY") == "" {
		for {
			// let one of the server acquire the lock, if not let them timeout.
			// which shall be retried again by this loop.
			if _, err := txnLk.GetLock(retryCtx, iamLockTimeout); err != nil {
				logger.Info("Waiting for all MinIO IAM sub-system to be initialized.. trying to acquire lock")
				time.Sleep(time.Duration(r.Float64() * float64(5*time.Second)))
				continue
			}

			if globalEtcdClient != nil {
			}

			// These messages only meant primarily for distributed setup, so only log during distributed setup.
			if globalIsDistErasure {
				logger.Info("Waiting for all MinIO IAM sub-system to be initialized.. lock acquired")
			}

			// Migrate IAM configuration, if necessary.
			if err := sys.doIAMConfigMigration(ctx); err != nil {
				txnLk.Unlock()
				if errors.Is(err, madmin.ErrMaliciousData) {
					logger.Fatal(err, "Unable to read encrypted IAM configuration. Please check your credentials.")
				}
				if configRetriableErrors(err) {
					logger.Info("Waiting for all MinIO IAM sub-system to be initialized.. possible cause (%v)", err)
					continue
				}
				logger.LogIf(ctx, fmt.Errorf("Unable to migrate IAM users and policies to new format: %w", err))
				logger.LogIf(ctx, errors.New("IAM sub-system is partially initialized, some users may not be available"))
				return
			}

			// Successfully migrated, proceed to load the users.
			txnLk.Unlock()
			break
		}
	}

	for {
		if err := sys.store.loadAll(ctx, sys); err != nil {
			if configRetriableErrors(err) {
				logger.Info("Waiting for all MinIO IAM sub-system to be initialized.. possible cause (%v)", err)
				time.Sleep(time.Duration(r.Float64() * float64(5*time.Second)))
				continue
			}
			if err != nil {
				logger.LogIf(ctx, fmt.Errorf("Unable to initialize IAM sub-system, some users may not be available %w", err))
			}
		}
		break
	}

	// Invalidate the old cred always, even upon error to avoid any leakage.
	globalOldCred = auth.Credentials{}
	go sys.store.watch(ctx, sys)

	logger.Info("IAM initialization complete")
}

// DeletePolicy - deletes a canned policy from backend or etcd.
func (sys *IAMSys) DeletePolicy(policyName string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if policyName == "" {
		return errInvalidArgument
	}

	sys.store.lock()
	defer sys.store.unlock()

	err := sys.store.deletePolicyDoc(context.Background(), policyName)
	if errors.Is(err, errNoSuchPolicy) {
		// Ignore error if policy is already deleted.
		err = nil
	}
	sys.Lock()
	delete(sys.iamPolicyDocsMap, policyName)
	sys.Unlock()

	// update iamUsersMap
	if err := sys.LoadAllTypeUsers(); err != nil {
		return err
	}
	// update iamUserPolicyMap
	if err := sys.LoadMappedPolicies(false); err != nil {
		return err
	}

	// update iamPolicyDocsMap
	if err := sys.loadPolicyDocs(); err != nil {
		return err
	}

	// update iamGroupsMap
	if err := sys.loadGroups(); err != nil {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	// Delete user-policy mappings that will no longer apply
	for u, mp := range sys.iamUserPolicyMap {
		pset := mp.policySet()
		if pset.Contains(policyName) {
			cr, ok := sys.iamUsersMap[u]
			if !ok {
				// This case can happen when an temporary account
				// is deleted or expired, removed it from userPolicyMap.
				delete(sys.iamUserPolicyMap, u)
				continue
			}
			pset.Remove(policyName)
			// User is from STS if the cred are temporary
			sys.Unlock()
			if cr.IsTemp() {
				sys.policyDBSet(u, strings.Join(pset.ToSlice(), ","), stsUser, false)
			} else {
				sys.policyDBSet(u, strings.Join(pset.ToSlice(), ","), regularUser, false)
			}
			sys.Lock()
		}
	}

	// Delete group-policy mappings that will no longer apply
	for g, mp := range sys.iamGroupPolicyMap {
		pset := mp.policySet()
		if pset.Contains(policyName) {
			pset.Remove(policyName)
			sys.Unlock()
			sys.policyDBSet(g, strings.Join(pset.ToSlice(), ","), regularUser, true)
			sys.Lock()
		}
	}

	return err
}

// InfoPolicy - expands the canned policy into its JSON structure.
func (sys *IAMSys) InfoPolicy(policyName string) (iampolicy.Policy, error) {
	if !sys.Initialized() {
		return iampolicy.Policy{}, errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()
	v, ok := sys.iamPolicyDocsMap[policyName]
	if !ok {
		return iampolicy.Policy{}, errNoSuchPolicy
	}

	return v, nil
}

// ListPolicies - lists all canned policies.
func (sys *IAMSys) ListPolicies() (map[string]iampolicy.Policy, error) {
	if !sys.Initialized() {
		return nil, errServerNotInitialized
	}

	<-sys.configLoaded

	sys.Lock()
	defer sys.Unlock()

	policyDocsMap := make(map[string]iampolicy.Policy, len(sys.iamPolicyDocsMap))
	for k, v := range sys.iamPolicyDocsMap {
		policyDocsMap[k] = v
	}

	return policyDocsMap, nil
}

// SetPolicy - sets a new name policy.
func (sys *IAMSys) SetPolicy(policyName string, p iampolicy.Policy) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if p.IsEmpty() || policyName == "" {
		return errInvalidArgument
	}

	sys.store.lock()
	defer sys.store.unlock()

	if err := sys.loadPolicyDocs(); err != nil {
		return err
	}
	if err := sys.store.savePolicyDoc(context.Background(), policyName, p); err != nil {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	sys.iamPolicyDocsMap[policyName] = p
	return nil
}

// DeleteUser - delete user (only for long-term users not STS users).
func (sys *IAMSys) DeleteUser(accessKey string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	// First we remove the user from their groups.
	userInfo, getErr := sys.GetUserInfo(accessKey)
	if getErr != nil {
		return getErr
	}

	for _, group := range userInfo.MemberOf {
		removeErr := sys.RemoveUsersFromGroup(group, []string{accessKey})
		if removeErr != nil {
			return removeErr
		}
	}

	// Next we can remove the user from memory and IAM store
	sys.store.lock()
	defer sys.store.unlock()

	sys.Lock()
	for _, u := range sys.iamUsersMap {
		// Delete any service accounts if any first.
		if u.IsServiceAccount() {
			if u.ParentUser == accessKey {
				_ = sys.store.deleteUserIdentity(context.Background(), u.AccessKey, srvAccUser)
				delete(sys.iamUsersMap, u.AccessKey)
			}
		}
		// Delete any associated STS users.
		if u.IsTemp() {
			if u.ParentUser == accessKey {
				_ = sys.store.deleteUserIdentity(context.Background(), u.AccessKey, stsUser)
				delete(sys.iamUsersMap, u.AccessKey)
			}
		}
	}
	sys.Unlock()

	// It is ok to ignore deletion error on the mapped policy
	sys.store.deleteMappedPolicy(context.Background(), accessKey, regularUser, false)
	err := sys.store.deleteUserIdentity(context.Background(), accessKey, regularUser)
	if errors.Is(err, errNoSuchUser) {
		// ignore if user is already deleted.
		err = nil
	}

	sys.Lock()
	delete(sys.iamUsersMap, accessKey)
	delete(sys.iamUserPolicyMap, accessKey)
	sys.Unlock()

	return err
}

// CurrentPolicies - returns comma separated policy string, from
// an input policy after validating if there are any current
// policies which exist on MinIO corresponding to the input.
func (sys *IAMSys) CurrentPolicies(policyName string) string {
	if !sys.Initialized() {
		return ""
	}

	sys.Lock()
	defer sys.Unlock()

	var policies []string
	mp := newMappedPolicy(policyName)
	for _, policy := range mp.toSlice() {
		_, found := sys.iamPolicyDocsMap[policy]
		if found {
			policies = append(policies, policy)
		}
	}
	return strings.Join(policies, ",")
}

// SetTempUser - set temporary user credentials, these credentials have an expiry.
func (sys *IAMSys) SetTempUser(accessKey string, cred auth.Credentials, policyName string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	ttl := int64(cred.Expiration.Sub(UTCNow()).Seconds())

	sys.store.lock()
	defer sys.store.unlock()

	// If OPA is not set we honor any policy claims for this
	// temporary user which match with pre-configured canned
	// policies for this server.
	if globalPolicyOPA == nil && policyName != "" {
		mp := newMappedPolicy(policyName)
		combinedPolicy := sys.GetCombinedPolicy(mp.toSlice()...)

		if combinedPolicy.IsEmpty() {
			return fmt.Errorf("specified policy %s, not found %w", policyName, errNoSuchPolicy)
		}

		if err := sys.store.saveMappedPolicy(context.Background(), accessKey, stsUser, false, mp, options{ttl: ttl}); err != nil {
			return err
		}
		sys.Lock()
		sys.iamUserPolicyMap[accessKey] = mp
		sys.Unlock()
	}

	u := newUserIdentity(cred)
	if err := sys.store.saveUserIdentity(context.Background(), accessKey, stsUser, u, options{ttl: ttl}); err != nil {
		return err
	}

	sys.Lock()
	sys.iamUsersMap[accessKey] = cred
	sys.Unlock()
	return nil
}

// ListUsers - list all users.
func (sys *IAMSys) ListUsers() (map[string]madmin.UserInfo, error) {
	if !sys.Initialized() {
		return nil, errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return nil, errIAMActionNotAllowed
	}

	<-sys.configLoaded

	sys.Lock()
	defer sys.Unlock()

	var users = make(map[string]madmin.UserInfo)

	for k, v := range sys.iamUsersMap {
		if !v.IsTemp() && !v.IsServiceAccount() {
			users[k] = madmin.UserInfo{
				PolicyName: sys.iamUserPolicyMap[k].Policies,
				Status: func() madmin.AccountStatus {
					if v.IsValid() {
						return madmin.AccountEnabled
					}
					return madmin.AccountDisabled
				}(),
			}
		}
	}

	return users, nil
}

// IsTempUser - returns if given key is a temporary user.
func (sys *IAMSys) IsTempUser(name string) (bool, string, error) {
	if !sys.Initialized() {
		return false, "", errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	cred, found := sys.iamUsersMap[name]
	if !found {
		return false, "", errNoSuchUser
	}

	if cred.IsTemp() {
		return true, cred.ParentUser, nil
	}

	return false, "", nil
}

// IsServiceAccount - returns if given key is a service account
func (sys *IAMSys) IsServiceAccount(name string) (bool, string, error) {
	if !sys.Initialized() {
		return false, "", errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	cred, found := sys.iamUsersMap[name]
	if !found {
		return false, "", errNoSuchUser
	}

	if cred.IsServiceAccount() {
		return true, cred.ParentUser, nil
	}

	return false, "", nil
}

// GetUserInfo - get info on a user.
func (sys *IAMSys) GetUserInfo(name string) (u madmin.UserInfo, err error) {
	if !sys.Initialized() {
		return u, errServerNotInitialized
	}

	select {
	case <-sys.configLoaded:
	default:
		sys.loadUserFromStore(name)
	}

	sys.Lock()
	defer sys.Unlock()

	if sys.usersSysType != MinIOUsersSysType {
		// If the user has a mapped policy or is a member of a group, we
		// return that info. Otherwise we return error.
		mappedPolicy, ok1 := sys.iamUserPolicyMap[name]
		memberships, ok2 := sys.iamUserGroupMemberships[name]
		//sys.Unlock()
		if !ok1 && !ok2 {
			return u, errNoSuchUser
		}
		return madmin.UserInfo{
			PolicyName: mappedPolicy.Policies,
			MemberOf:   memberships.ToSlice(),
		}, nil
	}

	cred, found := sys.iamUsersMap[name]
	if !found {
		return u, errNoSuchUser
	}

	if cred.IsTemp() || cred.IsServiceAccount() {
		return u, errIAMActionNotAllowed
	}

	return madmin.UserInfo{
		PolicyName: sys.iamUserPolicyMap[name].Policies,
		Status: func() madmin.AccountStatus {
			if cred.IsValid() {
				return madmin.AccountEnabled
			}
			return madmin.AccountDisabled
		}(),
		MemberOf: sys.iamUserGroupMemberships[name].ToSlice(),
	}, nil

}

// SetUserStatus - sets current user status, supports disabled or enabled.
func (sys *IAMSys) SetUserStatus(accessKey string, status madmin.AccountStatus) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	if status != madmin.AccountEnabled && status != madmin.AccountDisabled {
		return errInvalidArgument
	}

	sys.store.lock()
	defer sys.store.unlock()
	if err := sys.LoadUser(accessKey, regularUser); err != nil {
		return err
	}

	sys.Lock()
	cred, ok := sys.iamUsersMap[accessKey]
	sys.Unlock()
	if !ok {
		return errNoSuchUser
	}

	if cred.IsTemp() || cred.IsServiceAccount() {
		return errIAMActionNotAllowed
	}

	uinfo := newUserIdentity(auth.Credentials{
		AccessKey: accessKey,
		SecretKey: cred.SecretKey,
		Status: func() string {
			if status == madmin.AccountEnabled {
				return auth.AccountOn
			}
			return auth.AccountOff
		}(),
	})

	if err := sys.store.saveUserIdentity(context.Background(), accessKey, regularUser, uinfo); err != nil {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	sys.iamUsersMap[accessKey] = uinfo.Credentials
	return nil
}

type newServiceAccountOpts struct {
	sessionPolicy *iampolicy.Policy
	accessKey     string
	secretKey     string
}

// NewServiceAccount - create a new service account
func (sys *IAMSys) NewServiceAccount(ctx context.Context, parentUser string, groups []string, opts newServiceAccountOpts) (auth.Credentials, error) {
	if !sys.Initialized() {
		return auth.Credentials{}, errServerNotInitialized
	}

	var policyBuf []byte
	if opts.sessionPolicy != nil {
		err := opts.sessionPolicy.Validate()
		if err != nil {
			return auth.Credentials{}, err
		}
		policyBuf, err = json.Marshal(opts.sessionPolicy)
		if err != nil {
			return auth.Credentials{}, err
		}
		if len(policyBuf) > 16*humanize.KiByte {
			return auth.Credentials{}, fmt.Errorf("Session policy should not exceed 16 KiB characters")
		}
	}

	if parentUser == globalActiveCred.AccessKey {
		return auth.Credentials{}, errIAMActionNotAllowed
	}

	sys.store.lock()
	defer sys.store.unlock()
	if err := sys.LoadAllTypeUsers(); err != nil {
		return auth.Credentials{}, err
	}

	sys.Lock()
	cr, ok := sys.iamUsersMap[parentUser]
	if !ok {
		// For LDAP users we would need this fallback
		if sys.usersSysType != MinIOUsersSysType {
			_, ok = sys.iamUserPolicyMap[parentUser]
			if !ok {
				var found bool
				for _, group := range groups {
					_, ok = sys.iamGroupPolicyMap[group]
					if !ok {
						continue
					}
					found = true
					break
				}
				if !found {
					sys.Unlock()
					return auth.Credentials{}, errNoSuchUser
				}
			}
		}
	}
	sys.Unlock()

	// Disallow service accounts to further create more service accounts.
	if cr.IsServiceAccount() {
		return auth.Credentials{}, errIAMActionNotAllowed
	}

	m := make(map[string]interface{})
	m[parentClaim] = parentUser

	if len(policyBuf) > 0 {
		m[iampolicy.SessionPolicyName] = base64.StdEncoding.EncodeToString(policyBuf)
		m[iamPolicyClaimNameSA()] = "embedded-policy"
	} else {
		m[iamPolicyClaimNameSA()] = "inherited-policy"
	}

	var (
		cred auth.Credentials
		err  error
	)

	if len(opts.accessKey) > 0 {
		cred, err = auth.CreateNewCredentialsWithMetadata(opts.accessKey, opts.secretKey, m, globalActiveCred.SecretKey)
	} else {
		cred, err = auth.GetNewCredentialsWithMetadata(m, globalActiveCred.SecretKey)
	}
	if err != nil {
		return auth.Credentials{}, err
	}
	cred.ParentUser = parentUser
	cred.Groups = groups
	cred.Status = string(auth.AccountOn)

	u := newUserIdentity(cred)

	if err := sys.store.saveUserIdentity(context.Background(), u.Credentials.AccessKey, srvAccUser, u); err != nil {
		return auth.Credentials{}, err
	}
	sys.Lock()
	defer sys.Unlock()
	sys.iamUsersMap[u.Credentials.AccessKey] = u.Credentials

	return cred, nil
}

type updateServiceAccountOpts struct {
	sessionPolicy *iampolicy.Policy
	secretKey     string
	status        string
}

// UpdateServiceAccount - edit a service account
func (sys *IAMSys) UpdateServiceAccount(ctx context.Context, accessKey string, opts updateServiceAccountOpts) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	// lock disk config
	sys.store.lock()
	defer sys.store.unlock()

	if err := sys.LoadUser(accessKey, srvAccUser); err != nil {
		return err
	}

	sys.Lock()
	cr, ok := sys.iamUsersMap[accessKey]
	if !ok || !cr.IsServiceAccount() {
		sys.Unlock()
		return errNoSuchServiceAccount
	}

	if opts.secretKey != "" {
		if !auth.IsSecretKeyValid(opts.secretKey) {
			return auth.ErrInvalidSecretKeyLength
		}
		cr.SecretKey = opts.secretKey
	}

	if opts.status != "" {
		cr.Status = opts.status
	}
	sys.Unlock()

	if opts.sessionPolicy != nil {
		err := opts.sessionPolicy.Validate()
		if err != nil {
			return err
		}
		policyBuf, err := json.Marshal(opts.sessionPolicy)
		if err != nil {
			return err
		}
		if len(policyBuf) > 16*humanize.KiByte {
			return fmt.Errorf("Session policy should not exceed 16 KiB characters")
		}

		m := make(map[string]interface{})
		m[iampolicy.SessionPolicyName] = base64.StdEncoding.EncodeToString(policyBuf)
		m[iamPolicyClaimNameSA()] = "embedded-policy"
		m[parentClaim] = cr.ParentUser
		cr.SessionToken, err = auth.JWTSignWithAccessKey(accessKey, m, globalActiveCred.SecretKey)
		if err != nil {
			return err
		}
	}

	// update disk config
	u := newUserIdentity(cr)
	if err := sys.store.saveUserIdentity(context.Background(), u.Credentials.AccessKey, srvAccUser, u); err != nil {
		return err
	}

	// update cache
	sys.Lock()
	defer sys.Unlock()
	sys.iamUsersMap[u.Credentials.AccessKey] = u.Credentials

	return nil
}

// ListServiceAccounts - lists all services accounts associated to a specific user
func (sys *IAMSys) ListServiceAccounts(ctx context.Context, accessKey string) ([]auth.Credentials, error) {
	if !sys.Initialized() {
		return nil, errServerNotInitialized
	}

	<-sys.configLoaded

	sys.Lock()
	defer sys.Unlock()

	var serviceAccounts []auth.Credentials
	for _, v := range sys.iamUsersMap {
		if v.IsServiceAccount() && v.ParentUser == accessKey {
			// Hide secret key & session key here
			v.SecretKey = ""
			v.SessionToken = ""
			serviceAccounts = append(serviceAccounts, v)
		}
	}

	return serviceAccounts, nil
}

// GetServiceAccount - gets information about a service account
func (sys *IAMSys) GetServiceAccount(ctx context.Context, accessKey string) (auth.Credentials, *iampolicy.Policy, error) {
	if !sys.Initialized() {
		return auth.Credentials{}, nil, errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	sa, ok := sys.iamUsersMap[accessKey]
	if !ok || !sa.IsServiceAccount() {
		return auth.Credentials{}, nil, errNoSuchServiceAccount
	}

	var embeddedPolicy *iampolicy.Policy

	jwtClaims, err := auth.ExtractClaims(sa.SessionToken, globalActiveCred.SecretKey)
	if err == nil {
		pt, ptok := jwtClaims.Lookup(iamPolicyClaimNameSA())
		sp, spok := jwtClaims.Lookup(iampolicy.SessionPolicyName)
		if ptok && spok && pt == "embedded-policy" {
			policyBytes, err := base64.StdEncoding.DecodeString(sp)
			if err == nil {
				p, err := iampolicy.ParseConfig(bytes.NewReader(policyBytes))
				if err == nil {
					policy := iampolicy.Policy{}.Merge(*p)
					embeddedPolicy = &policy
				}
			}
		}
	}

	// Hide secret & session keys
	sa.SecretKey = ""
	sa.SessionToken = ""

	return sa, embeddedPolicy, nil
}

// DeleteServiceAccount - delete a service account
func (sys *IAMSys) DeleteServiceAccount(ctx context.Context, accessKey string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	sys.store.lock()
	defer sys.store.unlock()

	sys.Lock()
	sa, ok := sys.iamUsersMap[accessKey]
	sys.Unlock()
	if !ok || !sa.IsServiceAccount() {
		return nil
	}

	// It is ok to ignore deletion error on the mapped policy
	err := sys.store.deleteUserIdentity(context.Background(), accessKey, srvAccUser)
	if err != nil {
		// ignore if user is already deleted.
		if errors.Is(err, errNoSuchUser) {
			return nil
		}
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	delete(sys.iamUsersMap, accessKey)
	return nil
}

// CreateUser - create new user credentials and policy, if user already exists
// they shall be rewritten with new inputs.
func (sys *IAMSys) CreateUser(accessKey string, uinfo madmin.UserInfo) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	sys.store.lock()
	defer sys.store.unlock()
	if err := sys.LoadAllTypeUsers(); err != nil {
		return err
	}

	sys.Lock()
	cr, ok := sys.iamUsersMap[accessKey]
	sys.Unlock()
	if cr.IsTemp() && ok {
		return errIAMActionNotAllowed
	}

	u := newUserIdentity(auth.Credentials{
		AccessKey: accessKey,
		SecretKey: uinfo.SecretKey,
		Status: func() string {
			if uinfo.Status == madmin.AccountEnabled {
				return auth.AccountOn
			}
			return auth.AccountOff
		}(),
	})

	if err := sys.store.saveUserIdentity(context.Background(), accessKey, regularUser, u); err != nil {
		return err
	}

	sys.Lock()
	sys.iamUsersMap[accessKey] = u.Credentials
	sys.Unlock()
	// Set policy if specified.
	if uinfo.PolicyName != "" {
		if err := sys.LoadPolicyMapping(accessKey, regularUser, false); err != nil {
			return err
		}
		if err := sys.loadPolicyDocs(); err != nil {
			return err
		}
		return sys.policyDBSet(accessKey, uinfo.PolicyName, regularUser, false)
	}
	return nil
}

// SetUserSecretKey - sets user secret key
func (sys *IAMSys) SetUserSecretKey(accessKey string, secretKey string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	sys.store.lock()
	defer sys.store.unlock()
	if err := sys.LoadUser(accessKey, regularUser); err != nil {
		return err
	}
	sys.Lock()
	cred, ok := sys.iamUsersMap[accessKey]
	sys.Unlock()
	if !ok {
		return errNoSuchUser
	}

	cred.SecretKey = secretKey
	u := newUserIdentity(cred)
	if err := sys.store.saveUserIdentity(context.Background(), accessKey, regularUser, u); err != nil {
		return err
	}

	sys.Lock()
	defer sys.Unlock()
	sys.iamUsersMap[accessKey] = cred
	return nil
}

func (sys *IAMSys) loadUserFromStore(accessKey string) {
	sys.Lock()
	defer sys.Unlock()
	// If user is already found proceed.
	if _, found := sys.iamUsersMap[accessKey]; !found {
		//sys.store.loadUser(context.Background(), accessKey, regularUser, sys.iamUsersMap)
		sys.Unlock()
		sys.LoadUser(accessKey, regularUser)
		sys.Lock()
		if _, found = sys.iamUsersMap[accessKey]; found {
			// found user, load its mapped policies
			//sys.store.loadMappedPolicy(context.Background(), accessKey, regularUser, false, sys.iamUserPolicyMap)
			sys.Unlock()
			sys.LoadPolicyMapping(accessKey, regularUser, false)
			sys.Lock()
		} else {
			//sys.store.loadUser(context.Background(), accessKey, srvAccUser, sys.iamUsersMap)
			sys.Unlock()
			sys.LoadUser(accessKey, srvAccUser)
			sys.Lock()
			if svc, found := sys.iamUsersMap[accessKey]; found {
				sys.Unlock()
				// Found service account, load its parent user and its mapped policies.
				if sys.usersSysType == MinIOUsersSysType {
					//sys.store.loadUser(context.Background(), svc.ParentUser, regularUser, sys.iamUsersMap)
					sys.LoadUser(svc.ParentUser, regularUser)
				}
				//sys.store.loadMappedPolicy(context.Background(), svc.ParentUser, regularUser, false, sys.iamUserPolicyMap)
				sys.LoadPolicyMapping(svc.ParentUser, regularUser, false)
				sys.Lock()
			} else {
				// None found fall back to STS users.
				//sys.store.loadUser(context.Background(), accessKey, stsUser, sys.iamUsersMap)
				sys.Unlock()
				sys.LoadUser(accessKey, stsUser)
				sys.Lock()
				if _, found = sys.iamUsersMap[accessKey]; found {
					// STS user found, load its mapped policy.
					//sys.store.loadMappedPolicy(context.Background(), accessKey, stsUser, false, sys.iamUserPolicyMap)
					sys.Unlock()
					sys.LoadPolicyMapping(accessKey, stsUser, false)
					sys.Lock()
				}
			}
		}
	}

	// Load associated policies if any.
	for _, policy := range sys.iamUserPolicyMap[accessKey].toSlice() {
		if _, found := sys.iamPolicyDocsMap[policy]; !found {
			//sys.store.loadPolicyDoc(context.Background(), policy, sys.iamPolicyDocsMap)
			sys.Unlock()
			sys.LoadPolicy(policy)
			sys.Lock()
		}
	}

	sys.buildUserGroupMemberships()
}

// GetUser - get user credentials
func (sys *IAMSys) GetUser(accessKey string) (cred auth.Credentials, ok bool) {
	if !sys.Initialized() {
		return cred, false
	}

	fallback := false
	select {
	case <-sys.configLoaded:
	default:
		sys.loadUserFromStore(accessKey)
		fallback = true
	}

	sys.Lock()
	defer sys.Unlock()
	cred, ok = sys.iamUsersMap[accessKey]
	if !ok && !fallback {
		// accessKey not found, also
		// IAM store is not in fallback mode
		// we can try to reload again from
		// the IAM store and see if credential
		// exists now. If it doesn't proceed to
		// fail.
		sys.Unlock()
		sys.loadUserFromStore(accessKey)
		sys.Lock()
		cred, ok = sys.iamUsersMap[accessKey]
	}
	if ok && cred.IsValid() {
		if cred.ParentUser != "" && sys.usersSysType == MinIOUsersSysType {
			_, ok = sys.iamUsersMap[cred.ParentUser]
		}
		// for LDAP service accounts with ParentUser set
		// we have no way to validate, either because user
		// doesn't need an explicit policy as it can come
		// automatically from a group. We are safe to ignore
		// this and continue as policies would fail eventually
		// the policies are missing or not configured.
	}
	return cred, ok && cred.IsValid()
}

// AddUsersToGroup - adds users to a group, creating the group if
// needed. No error if user(s) already are in the group.
func (sys *IAMSys) AddUsersToGroup(group string, members []string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if group == "" {
		return errInvalidArgument
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	sys.store.lock()
	defer sys.store.unlock()

	if err := sys.LoadAllTypeUsers(); err != nil {
		return err
	}
	if err := sys.LoadGroup(group); err != nil {
		return err
	}

	sys.Lock()
	// Validate that all members exist.
	for _, member := range members {
		cr, ok := sys.iamUsersMap[member]
		if !ok {
			sys.Unlock()
			return errNoSuchUser
		}
		if cr.IsTemp() {
			sys.Unlock()
			return errIAMActionNotAllowed
		}
	}

	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		// Set group as enabled by default when it doesn't
		// exist.
		gi = newGroupInfo(members)
	} else {
		mergedMembers := append(gi.Members, members...)
		uniqMembers := set.CreateStringSet(mergedMembers...).ToSlice()
		gi.Members = uniqMembers
	}
	sys.Unlock()

	if err := sys.store.saveGroupInfo(context.Background(), group, gi); err != nil {
		return err
	}

	sys.Lock()
	sys.iamGroupsMap[group] = gi
	// update user-group membership map
	for _, member := range members {
		gset := sys.iamUserGroupMemberships[member]
		if gset == nil {
			gset = set.CreateStringSet(group)
		} else {
			gset.Add(group)
		}
		sys.iamUserGroupMemberships[member] = gset
	}
	sys.Unlock()

	return nil
}

// RemoveUsersFromGroup - remove users from group. If no users are
// given, and the group is empty, deletes the group as well.
func (sys *IAMSys) RemoveUsersFromGroup(group string, members []string) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	if group == "" {
		return errInvalidArgument
	}

	// lock all write config action
	sys.store.lock()
	defer sys.store.unlock()

	// update user cache
	if err := sys.LoadAllTypeUsers(); err != nil {
		return err
	}

	if err := sys.LoadGroup(group); err != nil {
		return err
	}

	sys.Lock()
	// Validate that all members exist.
	for _, member := range members {
		cr, ok := sys.iamUsersMap[member]
		if !ok {
			sys.Unlock()
			return errNoSuchUser
		}
		if cr.IsTemp() {
			sys.Unlock()
			return errIAMActionNotAllowed
		}
	}

	gi, ok := sys.iamGroupsMap[group]
	sys.Unlock()
	if !ok {
		return errNoSuchGroup
	}

	// Check if attempting to delete a non-empty group.
	if len(members) == 0 && len(gi.Members) != 0 {
		return errGroupNotEmpty
	}

	if len(members) == 0 {
		// len(gi.Members) == 0 here.

		// Remove the group from storage. First delete the
		// mapped policy. No-mapped-policy case is ignored.
		if err := sys.store.deleteMappedPolicy(context.Background(), group, regularUser, true); err != nil && !errors.Is(err, errNoSuchPolicy) {
			return err
		}
		if err := sys.store.deleteGroupInfo(context.Background(), group); err != nil && !errors.Is(err, errNoSuchGroup) {
			return err
		}

		sys.Lock()
		// Delete from server memory
		delete(sys.iamGroupsMap, group)
		delete(sys.iamGroupPolicyMap, group)
		sys.Unlock()
		return nil
	}

	// Only removing members.
	s := set.CreateStringSet(gi.Members...)
	d := set.CreateStringSet(members...)
	gi.Members = s.Difference(d).ToSlice()

	err := sys.store.saveGroupInfo(context.Background(), group, gi)
	if err != nil {
		return err
	}

	sys.Lock()
	sys.iamGroupsMap[group] = gi
	// update user-group membership map
	for _, member := range members {
		gset := sys.iamUserGroupMemberships[member]
		if gset == nil {
			continue
		}
		gset.Remove(group)
		sys.iamUserGroupMemberships[member] = gset
	}
	sys.Unlock()

	return nil
}

// SetGroupStatus - enable/disabled a group
func (sys *IAMSys) SetGroupStatus(group string, enabled bool) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return errIAMActionNotAllowed
	}

	if group == "" {
		return errInvalidArgument
	}

	sys.store.lock()
	defer sys.store.unlock()

	if err := sys.LoadGroup(group); err != nil {
		return err
	}
	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		return errNoSuchGroup
	}

	if enabled {
		gi.Status = statusEnabled
	} else {
		gi.Status = statusDisabled
	}

	if err := sys.store.saveGroupInfo(context.Background(), group, gi); err != nil {
		return err
	}
	sys.Lock()
	defer sys.Unlock()
	sys.iamGroupsMap[group] = gi
	return nil
}

// GetGroupDescription - builds up group description
func (sys *IAMSys) GetGroupDescription(group string) (gd madmin.GroupDesc, err error) {
	if !sys.Initialized() {
		return gd, errServerNotInitialized
	}

	ps, err := sys.PolicyDBGet(group, true)
	if err != nil {
		return gd, err
	}

	policy := strings.Join(ps, ",")

	if sys.usersSysType != MinIOUsersSysType {
		return madmin.GroupDesc{
			Name:   group,
			Policy: policy,
		}, nil
	}

	sys.Lock()
	defer sys.Unlock()

	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		return gd, errNoSuchGroup
	}

	return madmin.GroupDesc{
		Name:    group,
		Status:  gi.Status,
		Members: gi.Members,
		Policy:  policy,
	}, nil
}

// ListGroups - lists groups.
func (sys *IAMSys) ListGroups() (r []string, err error) {
	if !sys.Initialized() {
		return r, errServerNotInitialized
	}

	if sys.usersSysType != MinIOUsersSysType {
		return nil, errIAMActionNotAllowed
	}

	<-sys.configLoaded

	sys.Lock()
	defer sys.Unlock()

	r = make([]string, 0, len(sys.iamGroupsMap))
	for k := range sys.iamGroupsMap {
		r = append(r, k)
	}

	return r, nil
}

// PolicyDBSet - sets a policy for a user or group in the PolicyDB.
func (sys *IAMSys) PolicyDBSet(name, policy string, isGroup bool) error {
	if !sys.Initialized() {
		return errServerNotInitialized
	}

	sys.store.lock()
	defer sys.store.unlock()

	if sys.usersSysType == LDAPUsersSysType {
		return sys.policyDBSet(name, policy, stsUser, isGroup)
	}

	return sys.policyDBSet(name, policy, regularUser, isGroup)
}

// iamUsersMap  iamGroupsMap iamPolicyDocsMap
// policyDBSet - sets a policy for user in the policy db.
// If policy == "", then policy mapping is removed.
func (sys *IAMSys) policyDBSet(name, policyName string, userType IAMUserType, isGroup bool) error {
	if name == "" {
		return errInvalidArgument
	}

	sys.Lock()
	if sys.usersSysType == MinIOUsersSysType {
		if !isGroup {
			if _, ok := sys.iamUsersMap[name]; !ok {
				sys.Unlock()
				return errNoSuchUser
			}
		} else {
			if _, ok := sys.iamGroupsMap[name]; !ok {
				sys.Unlock()
				return errNoSuchGroup
			}
		}
	}
	sys.Unlock()

	// Handle policy mapping removal
	if policyName == "" {
		if sys.usersSysType == LDAPUsersSysType {
			// Add a fallback removal towards previous content that may come back
			// as a ghost user due to lack of delete, this change occurred
			// introduced in PR #11840
			sys.store.deleteMappedPolicy(context.Background(), name, regularUser, false)
		}
		err := sys.store.deleteMappedPolicy(context.Background(), name, userType, isGroup)
		if err != nil && !errors.Is(err, errNoSuchPolicy) {
			return err
		}
		sys.Lock()
		if !isGroup {
			delete(sys.iamUserPolicyMap, name)
		} else {
			delete(sys.iamGroupPolicyMap, name)
		}
		sys.Unlock()
		return nil
	}

	mp := newMappedPolicy(policyName)
	for _, policy := range mp.toSlice() {
		if _, found := sys.iamPolicyDocsMap[policy]; !found {
			logger.LogIf(GlobalContext, fmt.Errorf("%w: (%s)", errNoSuchPolicy, policy))
			return errNoSuchPolicy
		}
	}

	// Handle policy mapping set/update
	if err := sys.store.saveMappedPolicy(context.Background(), name, userType, isGroup, mp); err != nil {
		return err
	}
	sys.Lock()
	defer sys.Unlock()
	if !isGroup {
		sys.iamUserPolicyMap[name] = mp
	} else {
		sys.iamGroupPolicyMap[name] = mp
	}
	return nil
}

// PolicyDBGet - gets policy set on a user or group. If a list of groups is
// given, policies associated with them are included as well.
func (sys *IAMSys) PolicyDBGet(name string, isGroup bool, groups ...string) ([]string, error) {
	if !sys.Initialized() {
		return nil, errServerNotInitialized
	}

	if name == "" {
		return nil, errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	policies, err := sys.policyDBGet(name, isGroup)
	if err != nil {
		return nil, err
	}

	if !isGroup {
		for _, group := range groups {
			ps, err := sys.policyDBGet(group, true)
			if err != nil {
				return nil, err
			}
			policies = append(policies, ps...)
		}
	}

	return policies, nil
}

// This call assumes that caller has the sys.Lock().
//
// If a group is passed, it returns policies associated with the group.
//
// If a user is passed, it returns policies of the user along with any groups
// that the server knows the user is a member of.
//
// In LDAP users mode, the server does not store any group membership
// information in IAM (i.e sys.iam*Map) - this info is stored only in the STS
// generated credentials. Thus we skip looking up group memberships, user map,
// and group map and check the appropriate policy maps directly.
func (sys *IAMSys) policyDBGet(name string, isGroup bool) (policies []string, err error) {
	if isGroup {
		if sys.usersSysType == MinIOUsersSysType {
			g, ok := sys.iamGroupsMap[name]
			if !ok {
				return nil, errNoSuchGroup
			}

			// Group is disabled, so we return no policy - this
			// ensures the request is denied.
			if g.Status == statusDisabled {
				return nil, nil
			}
		}

		return sys.iamGroupPolicyMap[name].toSlice(), nil
	}

	var u auth.Credentials
	var ok bool
	if sys.usersSysType == MinIOUsersSysType {
		// When looking for a user's policies, we also check if the user
		// and the groups they are member of are enabled.

		u, ok = sys.iamUsersMap[name]
		if !ok {
			return nil, errNoSuchUser
		}
		if !u.IsValid() {
			return nil, nil
		}
	}

	mp, ok := sys.iamUserPolicyMap[name]
	if !ok {
		if u.ParentUser != "" {
			mp = sys.iamUserPolicyMap[u.ParentUser]
		}
	}

	// returned policy could be empty
	policies = append(policies, mp.toSlice()...)

	for _, group := range sys.iamUserGroupMemberships[name].ToSlice() {
		// Skip missing or disabled groups
		gi, ok := sys.iamGroupsMap[group]
		if !ok || gi.Status == statusDisabled {
			continue
		}

		policies = append(policies, sys.iamGroupPolicyMap[group].toSlice()...)
	}

	return policies, nil
}

// IsAllowedServiceAccount - checks if the given service account is allowed to perform
// actions. The permission of the parent user is checked first
func (sys *IAMSys) IsAllowedServiceAccount(args iampolicy.Args, parent string) bool {
	// Now check if we have a subject claim
	p, ok := args.Claims[parentClaim]
	if ok {
		parentInClaim, ok := p.(string)
		if !ok {
			// Reject malformed/malicious requests.
			return false
		}
		// The parent claim in the session token should be equal
		// to the parent detected in the backend
		if parentInClaim != parent {
			return false
		}
	} else {
		// This is needed so a malicious user cannot
		// use a leaked session key of another user
		// to widen its privileges.
		return false
	}

	// Check policy for this service account.
	svcPolicies, err := sys.PolicyDBGet(parent, false, args.Groups...)
	if err != nil {
		logger.LogIf(GlobalContext, err)
		return false
	}

	if len(svcPolicies) == 0 {
		return false
	}

	var availablePolicies []iampolicy.Policy

	// Policies were found, evaluate all of them.
	sys.Lock()
	for _, pname := range svcPolicies {
		p, found := sys.iamPolicyDocsMap[pname]
		if found {
			availablePolicies = append(availablePolicies, p)
		}
	}
	sys.Unlock()

	if len(availablePolicies) == 0 {
		return false
	}

	combinedPolicy := availablePolicies[0]
	for i := 1; i < len(availablePolicies); i++ {
		combinedPolicy.Statements = append(combinedPolicy.Statements,
			availablePolicies[i].Statements...)
	}

	parentArgs := args
	parentArgs.AccountName = parent

	saPolicyClaim, ok := args.Claims[iamPolicyClaimNameSA()]
	if !ok {
		return false
	}

	saPolicyClaimStr, ok := saPolicyClaim.(string)
	if !ok {
		// Sub policy if set, should be a string reject
		// malformed/malicious requests.
		return false
	}

	if saPolicyClaimStr == "inherited-policy" {
		return combinedPolicy.IsAllowed(parentArgs)
	}

	// Now check if we have a sessionPolicy.
	spolicy, ok := args.Claims[iampolicy.SessionPolicyName]
	if !ok {
		return false
	}

	spolicyStr, ok := spolicy.(string)
	if !ok {
		// Sub policy if set, should be a string reject
		// malformed/malicious requests.
		return false
	}

	// Check if policy is parseable.
	subPolicy, err := iampolicy.ParseConfig(bytes.NewReader([]byte(spolicyStr)))
	if err != nil {
		// Log any error in input session policy config.
		logger.LogIf(GlobalContext, err)
		return false
	}

	// Policy without Version string value reject it.
	if subPolicy.Version == "" {
		return false
	}

	return combinedPolicy.IsAllowed(parentArgs) && subPolicy.IsAllowed(parentArgs)
}

// IsAllowedLDAPSTS - checks for LDAP specific claims and values
func (sys *IAMSys) IsAllowedLDAPSTS(args iampolicy.Args, parentUser string) bool {
	parentInClaimIface, ok := args.Claims[ldapUser]
	if ok {
		parentInClaim, ok := parentInClaimIface.(string)
		if !ok {
			// ldap parentInClaim name is not a string reject it.
			return false
		}

		if parentInClaim != parentUser {
			// ldap claim has been modified maliciously reject it.
			return false
		}
	} else {
		// no ldap parentInClaim claim present reject it.
		return false
	}

	// Check policy for this LDAP user.
	ldapPolicies, err := sys.PolicyDBGet(parentUser, false, args.Groups...)
	if err != nil {
		return false
	}

	if len(ldapPolicies) == 0 {
		return false
	}

	var availablePolicies []iampolicy.Policy

	// Policies were found, evaluate all of them.
	sys.Lock()
	for _, pname := range ldapPolicies {
		p, found := sys.iamPolicyDocsMap[pname]
		if found {
			availablePolicies = append(availablePolicies, p)
		}
	}
	sys.Unlock()

	if len(availablePolicies) == 0 {
		return false
	}

	combinedPolicy := availablePolicies[0]
	for i := 1; i < len(availablePolicies); i++ {
		combinedPolicy.Statements =
			append(combinedPolicy.Statements,
				availablePolicies[i].Statements...)
	}

	return combinedPolicy.IsAllowed(args)
}

// IsAllowedSTS is meant for STS based temporary credentials,
// which implements claims validation and verification other than
// applying policies.
func (sys *IAMSys) IsAllowedSTS(args iampolicy.Args, parentUser string) bool {
	// If it is an LDAP request, check that user and group
	// policies allow the request.
	if sys.usersSysType == LDAPUsersSysType {
		return sys.IsAllowedLDAPSTS(args, parentUser)
	}

	policies, ok := args.GetPolicies(iamPolicyClaimNameOpenID())
	if !ok {
		// When claims are set, it should have a policy claim field.
		return false
	}

	// When claims are set, it should have policies as claim.
	if policies.IsEmpty() {
		// No policy, no access!
		return false
	}

	sys.Lock()
	defer sys.Unlock()

	// If policy is available for given user, check the policy.
	mp, ok := sys.iamUserPolicyMap[args.AccountName]
	if !ok {
		// No policy set for the user that we can find, no access!
		return false
	}

	if !policies.Equals(mp.policySet()) {
		// When claims has a policy, it should match the
		// policy of args.AccountName which server remembers.
		// if not reject such requests.
		return false
	}

	var availablePolicies []iampolicy.Policy
	for pname := range policies {
		p, found := sys.iamPolicyDocsMap[pname]
		if !found {
			// all policies presented in the claim should exist
			logger.LogIf(GlobalContext, fmt.Errorf("expected policy (%s) missing from the JWT claim %s, rejecting the request", pname, iamPolicyClaimNameOpenID()))
			return false
		}
		availablePolicies = append(availablePolicies, p)
	}

	combinedPolicy := availablePolicies[0]
	for i := 1; i < len(availablePolicies); i++ {
		combinedPolicy.Statements = append(combinedPolicy.Statements,
			availablePolicies[i].Statements...)
	}

	// Now check if we have a sessionPolicy.
	spolicy, ok := args.Claims[iampolicy.SessionPolicyName]
	if ok {
		spolicyStr, ok := spolicy.(string)
		if !ok {
			// Sub policy if set, should be a string reject
			// malformed/malicious requests.
			return false
		}

		// Check if policy is parseable.
		subPolicy, err := iampolicy.ParseConfig(bytes.NewReader([]byte(spolicyStr)))
		if err != nil {
			// Log any error in input session policy config.
			logger.LogIf(GlobalContext, err)
			return false
		}

		// Policy without Version string value reject it.
		if subPolicy.Version == "" {
			return false
		}

		// Sub policy is set and valid.
		return combinedPolicy.IsAllowed(args) && subPolicy.IsAllowed(args)
	}

	// Sub policy not set, this is most common since subPolicy
	// is optional, use the inherited policies.
	return combinedPolicy.IsAllowed(args)
}

// GetCombinedPolicy returns a combined policy combining all policies
func (sys *IAMSys) GetCombinedPolicy(policies ...string) iampolicy.Policy {
	// Policies were found, evaluate all of them.
	sys.Lock()
	defer sys.Unlock()

	var availablePolicies []iampolicy.Policy
	for _, pname := range policies {
		p, found := sys.iamPolicyDocsMap[pname]
		if found {
			availablePolicies = append(availablePolicies, p)
		}
	}

	if len(availablePolicies) == 0 {
		return iampolicy.Policy{}
	}

	combinedPolicy := availablePolicies[0]
	for i := 1; i < len(availablePolicies); i++ {
		combinedPolicy.Statements = append(combinedPolicy.Statements,
			availablePolicies[i].Statements...)
	}

	return combinedPolicy
}

// IsAllowed - checks given policy args is allowed to continue the Rest API.
func (sys *IAMSys) IsAllowed(args iampolicy.Args) bool {
	// If opa is configured, use OPA always.
	if globalPolicyOPA != nil {
		ok, err := globalPolicyOPA.IsAllowed(args)
		if err != nil {
			logger.LogIf(GlobalContext, err)
		}
		return ok
	}

	// Policies don't apply to the owner.
	if args.IsOwner {
		return true
	}

	// If the credential is temporary, perform STS related checks.
	ok, parentUser, err := sys.IsTempUser(args.AccountName)
	if err != nil {
		return false
	}
	if ok {
		return sys.IsAllowedSTS(args, parentUser)
	}

	// If the credential is for a service account, perform related check
	ok, parentUser, err = sys.IsServiceAccount(args.AccountName)
	if err != nil {
		return false
	}
	if ok {
		return sys.IsAllowedServiceAccount(args, parentUser)
	}

	// Continue with the assumption of a regular user
	policies, err := sys.PolicyDBGet(args.AccountName, false, args.Groups...)
	if err != nil {
		return false
	}

	if len(policies) == 0 {
		// No policy found.
		return false
	}

	// Policies were found, evaluate all of them.
	return sys.GetCombinedPolicy(policies...).IsAllowed(args)
}

// Set default canned policies only if not already overridden by users.
func setDefaultCannedPolicies(policies map[string]iampolicy.Policy) {
	_, ok := policies["writeonly"]
	if !ok {
		policies["writeonly"] = iampolicy.WriteOnly
	}
	_, ok = policies["readonly"]
	if !ok {
		policies["readonly"] = iampolicy.ReadOnly
	}
	_, ok = policies["readwrite"]
	if !ok {
		policies["readwrite"] = iampolicy.ReadWrite
	}
	_, ok = policies["consoleAdmin"]
	if !ok {
		policies["consoleAdmin"] = iampolicy.Admin
	}
}

// buildUserGroupMemberships - builds the memberships map. IMPORTANT:
// Assumes that sys.Lock is held by caller.
func (sys *IAMSys) buildUserGroupMemberships() {
	for group, gi := range sys.iamGroupsMap {
		sys.updateGroupMembershipsMap(group, &gi)
	}
}

// updateGroupMembershipsMap - updates the memberships map for a
// group. IMPORTANT: Assumes sys.Lock() is held by caller.
func (sys *IAMSys) updateGroupMembershipsMap(group string, gi *GroupInfo) {
	if gi == nil {
		return
	}
	for _, member := range gi.Members {
		v := sys.iamUserGroupMemberships[member]
		if v == nil {
			v = set.CreateStringSet(group)
		} else {
			v.Add(group)
		}
		sys.iamUserGroupMemberships[member] = v
	}
}

// removeGroupFromMembershipsMap - removes the group from every member
// in the cache. IMPORTANT: Assumes sys.Lock() is held by caller.
func (sys *IAMSys) removeGroupFromMembershipsMap(group string) {
	for member, groups := range sys.iamUserGroupMemberships {
		if !groups.Contains(group) {
			continue
		}
		groups.Remove(group)
		sys.iamUserGroupMemberships[member] = groups
	}
}

// EnableLDAPSys - enable ldap system users type.
func (sys *IAMSys) EnableLDAPSys() {
	sys.usersSysType = LDAPUsersSysType
}

func (sys *IAMSys) loadPolicyDocs() error {
	m := make(map[string]iampolicy.Policy)
	if err := sys.store.loadPolicyDocs(context.Background(), m); err != nil {
		return err
	}

	// Sets default canned policies, if none are set.
	setDefaultCannedPolicies(m)
	sys.Lock()
	defer sys.Unlock()
	sys.iamPolicyDocsMap = m
	return nil
}

func (sys *IAMSys) loadGroups() error {
	m := make(map[string]GroupInfo)
	if err := sys.store.loadGroups(context.Background(), m); err != nil {
		return err
	}
	sys.Lock()
	defer sys.Unlock()
	sys.iamGroupsMap = m
	return nil
}

// NewIAMSys - creates new config system object.
func NewIAMSys() *IAMSys {
	return &IAMSys{
		usersSysType:            MinIOUsersSysType,
		iamUsersMap:             make(map[string]auth.Credentials),
		iamPolicyDocsMap:        make(map[string]iampolicy.Policy),
		iamUserPolicyMap:        make(map[string]MappedPolicy),
		iamGroupPolicyMap:       make(map[string]MappedPolicy),
		iamGroupsMap:            make(map[string]GroupInfo),
		iamUserGroupMemberships: make(map[string]set.StringSet),
		configLoaded:            make(chan struct{}),
	}
}
