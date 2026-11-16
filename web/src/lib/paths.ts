export function validName(name: string): boolean {
  return (
    name.trim().length > 0 && name !== '.' && name !== '..' && !/[\/\\\x00-\x1f\x7f]/.test(name)
  )
}
export function joinPath(parent: string, name: string): string {
  return (parent === '/' ? '' : parent) + '/' + name
}
export function parentPath(path: string): string {
  return path.slice(0, path.lastIndexOf('/')) || '/'
}
export function validDestination(path: string): boolean {
  return path.startsWith('/') && path !== '/' && path.split('/').slice(1).every(validName)
}
