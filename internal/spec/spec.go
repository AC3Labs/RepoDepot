// Package spec is the wire format between The Repo Depot server and its
// agents. The server decides what to install and where; the agent on the
// target machine carries it out and reports back.
package spec

// Install methods.
const (
	MethodCompose = "compose" // a Docker Compose project from the repo's source
	MethodRelease = "release" // a file attached to a GitHub release
	MethodSource  = "source"  // the repo's source at a ref, plus an optional build command
)

// Job actions.
const (
	ActionInstall   = "install"
	ActionUpdate    = "update"
	ActionUninstall = "uninstall"
)

// Job is one thing for an agent to do. Everything the agent needs is in
// here; the agent never talks to the server or GitHub's API on its own,
// only downloads the URLs it's given.
type Job struct {
	Action string `json:"action"`
	Method string `json:"method"`
	App    string `json:"app"` // short unique name, used for folders, services and compose projects
	Repo   string `json:"repo"`
	Ref    string `json:"ref"` // tag or commit being installed
	Dir    string `json:"dir"` // install folder on the agent's machine

	// SourceURL is a tarball of the repo at Ref (compose and source methods).
	SourceURL string `json:"source_url,omitempty"`
	// AssetURL / AssetName are the release file (release method).
	AssetURL  string `json:"asset_url,omitempty"`
	AssetName string `json:"asset_name,omitempty"`
	// Token, when set, is sent to GitHub with downloads (private repos,
	// rate limits). Never logged.
	Token string `json:"token,omitempty"`

	ComposeFile string `json:"compose_file,omitempty"` // which compose file in the source, e.g. docker-compose.yml
	// GeneratedCompose, when set, is written as ComposeFile first: for
	// repos with only a Dockerfile, the server writes a compose file.
	GeneratedCompose string `json:"generated_compose,omitempty"`
	ComposeOverride  string `json:"compose_override,omitempty"` // YAML merged on top, written as repodepot.override.yml
	Env              string `json:"env,omitempty"`              // written as .env next to the compose file

	PostInstall string `json:"post_install,omitempty"` // shell command run in the install folder after install/update

	Service *Service `json:"service,omitempty"` // run the installed program as a system service

	// LinkBinary puts a single installed program on the PATH (Linux:
	// /usr/local/bin/<name>).
	LinkBinary string `json:"link_binary,omitempty"`

	RemoveData bool `json:"remove_data,omitempty"` // uninstall: also delete the install folder and volumes
}

// Service runs the installed program in the background and at boot.
type Service struct {
	Exec string `json:"exec"` // program, relative to the install folder's app/ (or absolute)
	Args string `json:"args,omitempty"`
}

// JobState is what an agent reports about a job.
type JobState struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"` // running, done, error
	Log      []string `json:"log"`
	Error    string   `json:"error,omitempty"`
	Location string   `json:"location,omitempty"` // where it ended up, for display
	Started  string   `json:"started"`
	Finished string   `json:"finished,omitempty"`
}

// Health is GET /api/health on an agent.
type Health struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	Docker   bool   `json:"docker"`   // docker and docker compose are usable
	AppsDir  string `json:"apps_dir"` // default install root
	Admin    bool   `json:"admin"`    // running with root/administrator rights
}

// Manifest is written into every install folder (.repodepot.json) so an
// uninstall removes exactly what the install created.
type Manifest struct {
	App       string   `json:"app"`
	Repo      string   `json:"repo"`
	Method    string   `json:"method"`
	Ref       string   `json:"ref"`
	Compose   string   `json:"compose,omitempty"` // compose file used
	Project   string   `json:"project,omitempty"` // compose project name
	Service   string   `json:"service,omitempty"` // system service name
	Package   string   `json:"package,omitempty"` // .deb package name / .msi file kept for uninstall
	Links     []string `json:"links,omitempty"`   // symlinks created outside the folder
	Installed string   `json:"installed"`
	// Removed is set when the app was uninstalled but its data was kept;
	// the folder is still The Repo Depot's to delete later.
	Removed bool `json:"removed,omitempty"`
}
