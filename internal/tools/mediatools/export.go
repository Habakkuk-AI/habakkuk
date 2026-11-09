package mediatools

// ModelImage checks that data is an image a model can read (JPEG, PNG, GIF
// or WebP) and scales it down when it is too large to send, as view_image
// does. note describes the image and any scaling, for a tool result.
func ModelImage(name string, data []byte) (out []byte, mediaType, note string, err error) {
	return modelImage(name, data)
}
