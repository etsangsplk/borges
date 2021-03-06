package borges

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/src-d/go-git-fixtures"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"gopkg.in/src-d/go-billy.v3/memfs"
	"gopkg.in/src-d/go-billy.v3/osfs"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/storage/filesystem"
)

func TestNewGitReferencer(t *testing.T) {
	fixtures.Init()
	defer fixtures.Clean()

	for _, ct := range ChangesFixtures {
		t.Run(ct.TestName, func(t *testing.T) {
			assert := assert.New(t)
			r, err := ct.NewRepository()
			assert.NoError(err)

			gitRefs := NewGitReferencer(r)
			resGitRefs, err := gitRefs.References()
			assert.NoError(err)
			assert.Equal(len(ct.NewReferences), len(resGitRefs))

			resGitRefsByName := refsByName(resGitRefs)
			expectedRefsByName := refsByName(ct.NewReferences)
			for name, expectedRef := range expectedRefsByName {
				obtainedRef, ok := resGitRefsByName[name]
				assert.True(ok)
				assert.Equal(expectedRef.Name, obtainedRef.Name)
				assert.Equal(expectedRef.Hash, obtainedRef.Hash)
				assert.Equal(expectedRef.Init, obtainedRef.Init)
				assert.Equal(expectedRef.Roots, obtainedRef.Roots)
			}
		})
	}
}

func TestNewGitReferencer_ReferenceToTagObject(t *testing.T) {
	fixtures.Init()
	defer fixtures.Clean()
	require := require.New(t)

	srcFs := fixtures.ByTag("tags").One().DotGit()
	sto, err := filesystem.NewStorage(srcFs)
	require.NoError(err)

	r, err := git.Open(sto, memfs.New())
	require.NoError(err)

	newRefs := NewGitReferencer(r)
	refs, err := newRefs.References()
	require.NoError(err)
	require.Len(refs, 4)
	for _, ref := range refs {
		require.Equal("f7b877701fbf855b44c0a9e86f3fdce2c298b07f", ref.Init.String())
	}
}

func TestTemporaryCloner(t *testing.T) {
	suite.Run(t, new(TemporaryClonerSuite))
}

type TemporaryClonerSuite struct {
	suite.Suite
	tmpDir string
	cloner TemporaryCloner
}

func (s *TemporaryClonerSuite) SetupTest() {
	require := s.Require()
	err := fixtures.Init()
	require.NoError(err)

	s.tmpDir, err = ioutil.TempDir("", "borges-test")
	require.NoError(err)

	tmpFs := osfs.New(s.tmpDir)
	s.cloner = NewTemporaryCloner(tmpFs)
}

func (s *TemporaryClonerSuite) TearDownTest() {
	require := s.Require()
	fixtures.Clean()
	err := os.RemoveAll(s.tmpDir)
	require.NoError(err)
}

func (s *TemporaryClonerSuite) TestCloneRepository() {
	s.testBasicRepository("https://github.com/git-fixtures/basic.git")
	s.testBasicRepository("git://github.com/git-fixtures/basic.git")
}

func (s *TemporaryClonerSuite) testBasicRepository(url string) {
	require := s.Require()
	gr, err := s.cloner.Clone("foo", url)
	require.NoError(err)
	refs, err := gr.References()
	require.NoError(err)
	require.Len(refs, 3)
	err = gr.Close()
	require.NoError(err)
}

func (s *TemporaryClonerSuite) TestCloneEmptyRepository() {
	s.testEmptyRepository("https://github.com/git-fixtures/empty.git")
	s.testEmptyRepository("git://github.com/git-fixtures/empty.git")
}

func (s *TemporaryClonerSuite) testEmptyRepository(url string) {
	require := s.Require()
	gr, err := s.cloner.Clone("foo", url)
	require.NoError(err)
	refs, err := gr.References()
	require.NoError(err)
	require.Len(refs, 0)
	err = gr.Close()
	require.NoError(err)
}

func (s *TemporaryClonerSuite) TestCloneNonExistentRepository() {
	s.testNonExistentRepository("https://github.com/git-fixtures/non-existent.git")
	s.testNonExistentRepository("git//github.com/git-fixtures/non-existent.git")
}

func (s *TemporaryClonerSuite) testNonExistentRepository(url string) {
	require := s.Require()
	gr, err := s.cloner.Clone("foo", url)
	require.True(err == transport.ErrAuthenticationRequired ||
		err == transport.ErrRepositoryNotFound)

	require.Nil(gr)
}
