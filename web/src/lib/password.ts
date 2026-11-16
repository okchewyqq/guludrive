// bcrypt accepts at most 72 UTF-8 bytes, not 72 JavaScript characters.
export function validPasswordLength(password: string): boolean {
  const length = new TextEncoder().encode(password).byteLength
  return length > 0 && length <= 72
}
