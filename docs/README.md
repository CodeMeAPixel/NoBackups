# NoBackups docs

The [Mintlify](https://mintlify.com) documentation site for NoBackups.

```sh
make docs         # live preview at http://localhost:3000 (from the repo root)
make docs-check   # validate the build and check for broken links
```

Pages are `.mdx` files; navigation lives in `docs.json`. To publish, connect this repository in the Mintlify dashboard and set the docs directory to `docs/`.
