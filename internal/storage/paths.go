package storage

import "path"

// ProjectPrefix returns the canonical storage prefix for a user project.
func ProjectPrefix(userID, projectID string) string {
	return path.Join("users", userID, "projects", projectID)
}

// ProjectPrefixWithSlash returns ProjectPrefix with a trailing slash for prefix deletes and listings.
func ProjectPrefixWithSlash(userID, projectID string) string {
	return ProjectPrefix(userID, projectID) + "/"
}

// UserProjectsPrefix returns the prefix for listing projects under a user.
func UserProjectsPrefix(userID string) string {
	return path.Join("users", userID, "projects") + "/"
}

// ProjectObjectPath returns an absolute object path under a project prefix.
func ProjectObjectPath(userID, projectID, relPath string) string {
	return path.Join(ProjectPrefix(userID, projectID), relPath)
}