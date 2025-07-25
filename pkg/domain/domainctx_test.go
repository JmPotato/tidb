// Copyright 2015 PingCAP, Inc.
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

package domain

import (
	"testing"

	"github.com/pingcap/tidb/pkg/util/mock"
	"github.com/stretchr/testify/require"
)

func TestDomainCtx(t *testing.T) {
	ctx := mock.NewContext()
	ctx.BindDomainAndSchValidator(nil, nil)
	v := GetDomain(ctx)
	require.Nil(t, v)

	ctx.BindDomainAndSchValidator(&Domain{}, nil)
	v = GetDomain(ctx)
	require.NotNil(t, v)
}
