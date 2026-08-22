export default {
  extends: ['@commitlint/config-conventional'],
  rules: {
    // CLAUDE.md's allowed types.
    'type-enum': [2, 'always', ['feat', 'fix', 'docs', 'chore', 'ci', 'infra', 'test']],
    // Squash merges put the PR body into the commit body; PR prose should not
    // be constrained to commit-style line lengths.
    'body-max-line-length': [0],
    'footer-max-line-length': [0],
  },
};
