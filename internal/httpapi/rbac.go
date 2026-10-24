package httpapi

// What the API server may do in the cluster, as controller-gen markers: make generate renders
// them into config/rbac/apiserver/role.yaml (the ClusterRole consonance-apiserver). Each line is
// what the handlers call, and the lines are checked against it by cmd/apiserver's test, which
// runs the binary as a user bound to that role alone.
//
// Namespaces are never cached, so they need no list or watch: they are read to check that a
// namespace is the API server's own, created for a new project and deleted with it.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;delete
// Projects and Branches are read from the cache (get, list and watch for its informers); the
// API server creates Projects, and creates, updates (stop, start, rollback) and deletes Branches.
// +kubebuilder:rbac:groups=consonance.io,resources=projects,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=consonance.io,resources=branches,verbs=get;list;watch;create;update;delete
// Secrets are read uncached, one at a time: db-credentials and internal-token.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
