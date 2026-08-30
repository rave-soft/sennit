package azure

import "testing"

// TestNew_RequiresBaseURL guards against azure.New silently falling through
// to openai.New's DefaultURL (api.openai.com) when no base URL is
// configured: without this check the Azure key is sent to OpenAI's public
// API and the resulting 401 reads to the user as a bad key rather than a
// missing endpoint.
func TestNew_RequiresBaseURL(t *testing.T) {
	_, err := New(WithAPIKey("test-key"))
	if err == nil {
		t.Fatal("expected an error when no base URL is configured, got nil")
	}
}

func TestNew_AcceptsBaseURL(t *testing.T) {
	_, err := New(WithBaseURL("https://my-resource.openai.azure.com"), WithAPIKey("test-key"))
	if err != nil {
		t.Fatalf("expected no error with a base URL set, got %v", err)
	}
}
