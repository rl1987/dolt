// Copyright 2019 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package actions

import (
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dolt/go/libraries/doltcore/branch_control"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"
)

var ErrAlreadyExists = errors.New("already exists")
var ErrCOBranchDelete = errors.New("attempted to delete checked out branch")
var ErrUnmergedBranch = errors.New("branch is not fully merged")
var ErrWorkingSetsOnBothBranches = errors.New("checkout would overwrite uncommitted changes on target branch")

func RenameBranch(ctx context.Context, dbData env.DbData, oldBranch, newBranch string, remoteDbPro env.RemoteDbProvider, force bool, rsc *doltdb.ReplicationStatusController) error {
	oldRef := ref.NewBranchRef(oldBranch)
	newRef := ref.NewBranchRef(newBranch)

	// TODO: This function smears the branch updates across multiple commits of the datas.Database.

	err := CopyBranchOnDB(ctx, dbData.Ddb, oldBranch, newBranch, force, rsc)
	if err != nil {
		return err
	}

	headRef, err := dbData.Rsr.CWBHeadRef()
	if err != nil {
		return err
	}
	if ref.Equals(headRef, oldRef) {
		err = dbData.Rsw.SetCWBHeadRef(ctx, ref.MarshalableRef{Ref: newRef})
		if err != nil {
			return err
		}
	}

	fromWSRef, err := ref.WorkingSetRefForHead(oldRef)
	if err != nil {
		if !errors.Is(err, ref.ErrWorkingSetUnsupported) {
			return err
		}
	} else {
		toWSRef, err := ref.WorkingSetRefForHead(newRef)
		if err != nil {
			return err
		}
		// We always `force` here, because the CopyBranch up
		// above created a new branch and it will have a
		// working set.
		err = dbData.Ddb.CopyWorkingSet(ctx, fromWSRef, toWSRef, true /* force */)
		if err != nil {
			return err
		}
	}

	return DeleteBranch(ctx, dbData, oldBranch, DeleteOptions{Force: true}, remoteDbPro, rsc)
}

func CopyBranch(ctx context.Context, dEnv *env.DoltEnv, oldBranch, newBranch string, force bool) error {
	return CopyBranchOnDB(ctx, dEnv.DoltDB, oldBranch, newBranch, force, nil)
}

func CopyBranchOnDB(ctx context.Context, ddb *doltdb.DoltDB, oldBranch, newBranch string, force bool, rsc *doltdb.ReplicationStatusController) error {
	oldRef := ref.NewBranchRef(oldBranch)
	newRef := ref.NewBranchRef(newBranch)

	hasOld, oldErr := ddb.HasRef(ctx, oldRef)

	if oldErr != nil {
		return oldErr
	}

	hasNew, newErr := ddb.HasRef(ctx, newRef)

	if newErr != nil {
		return newErr
	}

	if !hasOld {
		return doltdb.ErrBranchNotFound
	} else if !force && hasNew {
		return ErrAlreadyExists
	} else if !doltdb.IsValidUserBranchName(newBranch) {
		return doltdb.ErrInvBranchName
	}

	cs, _ := doltdb.NewCommitSpec(oldBranch)
	cm, err := ddb.Resolve(ctx, cs, nil)

	if err != nil {
		return err
	}

	return ddb.NewBranchAtCommit(ctx, newRef, cm, rsc)
}

type DeleteOptions struct {
	Force  bool
	Remote bool
}

func DeleteBranch(ctx context.Context, dbData env.DbData, brName string, opts DeleteOptions, remoteDbPro env.RemoteDbProvider, rsc *doltdb.ReplicationStatusController) error {
	var branchRef ref.DoltRef
	if opts.Remote {
		var err error
		branchRef, err = ref.NewRemoteRefFromPathStr(brName)
		if err != nil {
			return err
		}
	} else {
		branchRef = ref.NewBranchRef(brName)
		headRef, err := dbData.Rsr.CWBHeadRef()
		if err != nil {
			return err
		}
		if ref.Equals(headRef, branchRef) {
			return ErrCOBranchDelete
		}
	}

	return DeleteBranchOnDB(ctx, dbData, branchRef, opts, remoteDbPro, rsc)
}

func DeleteBranchOnDB(ctx context.Context, dbdata env.DbData, branchRef ref.DoltRef, opts DeleteOptions, pro env.RemoteDbProvider, rsc *doltdb.ReplicationStatusController) error {
	ddb := dbdata.Ddb
	hasRef, err := ddb.HasRef(ctx, branchRef)

	if err != nil {
		return err
	} else if !hasRef {
		return doltdb.ErrBranchNotFound
	}

	if !opts.Force && !opts.Remote {
		// check to see if the branch is fully merged into its parent
		trackedBranches, err := dbdata.Rsr.GetBranches()
		if err != nil {
			return err
		}

		trackedBranch, hasUpstream := trackedBranches[branchRef.GetPath()]
		if hasUpstream {
			err = validateBranchMergedIntoUpstream(ctx, dbdata, branchRef, trackedBranch.Remote, pro)
			if err != nil {
				return err
			}
		} else {
			err = validateBranchMergedIntoCurrentWorkingBranch(ctx, dbdata, branchRef)
			if err != nil {
				return err
			}
		}
	}

	wsRef, err := ref.WorkingSetRefForHead(branchRef)
	if err != nil {
		if !errors.Is(err, ref.ErrWorkingSetUnsupported) {
			return err
		}
	} else {
		err = ddb.DeleteWorkingSet(ctx, wsRef)
		if err != nil {
			return err
		}
	}

	return ddb.DeleteBranch(ctx, branchRef, rsc)
}

// validateBranchMergedIntoCurrentWorkingBranch returns an error if the given branch is not fully merged into the HEAD of the current branch.
func validateBranchMergedIntoCurrentWorkingBranch(ctx context.Context, dbdata env.DbData, branch ref.DoltRef) error {
	branchSpec, err := doltdb.NewCommitSpec(branch.GetPath())
	if err != nil {
		return err
	}

	branchHead, err := dbdata.Ddb.Resolve(ctx, branchSpec, nil)
	if err != nil {
		return err
	}

	cwbCs, err := doltdb.NewCommitSpec("HEAD")
	if err != nil {
		return err
	}

	headRef, err := dbdata.Rsr.CWBHeadRef()
	if err != nil {
		return err
	}
	cwbHead, err := dbdata.Ddb.Resolve(ctx, cwbCs, headRef)
	if err != nil {
		return err
	}

	isMerged, err := branchHead.CanFastForwardTo(ctx, cwbHead)
	if err != nil {
		if errors.Is(err, doltdb.ErrUpToDate) {
			return nil
		}
		if errors.Is(err, doltdb.ErrIsAhead) {
			return ErrUnmergedBranch
		}

		return err
	}

	if !isMerged {
		return ErrUnmergedBranch
	}

	return nil
}

// validateBranchMergedIntoUpstream returns an error if the branch provided is not fully merged into its upstream
func validateBranchMergedIntoUpstream(ctx context.Context, dbdata env.DbData, branch ref.DoltRef, remoteName string, pro env.RemoteDbProvider) error {
	remotes, err := dbdata.Rsr.GetRemotes()
	if err != nil {
		return err
	}
	remote, ok := remotes[remoteName]
	if !ok {
		// TODO: skip error?
		return fmt.Errorf("remote %s not found", remoteName)
	}

	remoteDb, err := pro.GetRemoteDB(ctx, dbdata.Ddb.ValueReadWriter().Format(), remote, false)
	if err != nil {
		return err
	}

	cs, err := doltdb.NewCommitSpec(branch.GetPath())
	if err != nil {
		return err
	}

	remoteBranchHead, err := remoteDb.Resolve(ctx, cs, nil)
	if err != nil {
		return err
	}

	localBranchHead, err := dbdata.Ddb.Resolve(ctx, cs, nil)
	if err != nil {
		return err
	}

	canFF, err := localBranchHead.CanFastForwardTo(ctx, remoteBranchHead)
	if err != nil {
		if errors.Is(err, doltdb.ErrUpToDate) {
			return nil
		}
		if errors.Is(err, doltdb.ErrIsAhead) {
			return ErrUnmergedBranch
		}
		return err
	}

	if !canFF {
		return ErrUnmergedBranch
	}

	return nil
}

func CreateBranchWithStartPt(ctx context.Context, dbData env.DbData, newBranch, startPt string, force bool, rsc *doltdb.ReplicationStatusController) error {
	err := createBranch(ctx, dbData, newBranch, startPt, force, rsc)

	if err != nil {
		if err == ErrAlreadyExists {
			return fmt.Errorf("fatal: A branch named '%s' already exists.", newBranch)
		} else if err == doltdb.ErrInvBranchName {
			return fmt.Errorf("fatal: '%s' is an invalid branch name.", newBranch)
		} else if err == doltdb.ErrInvHash || doltdb.IsNotACommit(err) {
			return fmt.Errorf("fatal: '%s' is not a commit and a branch '%s' cannot be created from it", startPt, newBranch)
		} else {
			return fmt.Errorf("fatal: Unexpected error creating branch '%s' : %v", newBranch, err)
		}
	}
	err = branch_control.AddAdminForContext(ctx, newBranch)
	if err != nil {
		return err
	}

	return nil
}

func CreateBranchOnDB(ctx context.Context, ddb *doltdb.DoltDB, newBranch, startingPoint string, force bool, headRef ref.DoltRef, rsc *doltdb.ReplicationStatusController) error {
	branchRef := ref.NewBranchRef(newBranch)
	hasRef, err := ddb.HasRef(ctx, branchRef)
	if err != nil {
		return err
	}

	if !force && hasRef {
		return ErrAlreadyExists
	}

	if !doltdb.IsValidUserBranchName(newBranch) {
		return doltdb.ErrInvBranchName
	}

	cs, err := doltdb.NewCommitSpec(startingPoint)
	if err != nil {
		return err
	}

	cm, err := ddb.Resolve(ctx, cs, headRef)
	if err != nil {
		return err
	}

	err = ddb.NewBranchAtCommit(ctx, branchRef, cm, rsc)
	if err != nil {
		return err
	}

	return nil
}

func createBranch(ctx context.Context, dbData env.DbData, newBranch, startingPoint string, force bool, rsc *doltdb.ReplicationStatusController) error {
	headRef, err := dbData.Rsr.CWBHeadRef()
	if err != nil {
		return err
	}
	return CreateBranchOnDB(ctx, dbData.Ddb, newBranch, startingPoint, force, headRef, rsc)
}

var emptyHash = hash.Hash{}

func IsBranch(ctx context.Context, ddb *doltdb.DoltDB, str string) (bool, error) {
	return IsBranchOnDB(ctx, ddb, str)
}

func IsBranchOnDB(ctx context.Context, ddb *doltdb.DoltDB, str string) (bool, error) {
	dref := ref.NewBranchRef(str)
	return ddb.HasRef(ctx, dref)
}

func MaybeGetCommit(ctx context.Context, dEnv *env.DoltEnv, str string) (*doltdb.Commit, error) {
	cs, err := doltdb.NewCommitSpec(str)

	if err == nil {
		headRef, err := dEnv.RepoStateReader().CWBHeadRef()
		if err != nil {
			return nil, err
		}
		cm, err := dEnv.DoltDB.Resolve(ctx, cs, headRef)

		if errors.Is(err, doltdb.ErrBranchNotFound) {
			return nil, nil
		}

		switch err {
		case nil:
			return cm, nil

		case doltdb.ErrHashNotFound, doltdb.ErrBranchNotFound:
			return nil, nil

		default:
			return nil, err
		}
	}

	return nil, nil
}
